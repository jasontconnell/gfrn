package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var defaultIgnores = ".vs,.git"
var GOPROCESSES int = 48

func main() {
	wd := flag.String("dir", "", "working directory")
	f := flag.String("f", "", "what to find")
	r := flag.String("r", "", "what to replace it with")
	i := flag.String("i", ".vs,.git", "folders to ignore")
	c := flag.Bool("c", false, "case sensitive?")
	exts := flag.String("exts", "", "text file extensions")
	flag.Parse()

	if *wd == "" || *f == "" || *exts == "" {
		fmt.Println("Dir, Find and Exts must be specified and non-blank")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if !strings.HasPrefix(*i, defaultIgnores) {
		*i = defaultIgnores + *i
	}

	start := time.Now()

	err := run(*wd, *f, *r, *i, *exts, *c)
	if err != nil {
		fmt.Println("Couldn't do it man", err)
	}

	fmt.Println("Finished", time.Since(start))
}

func run(dir, find, replace, ignoredirs, textExtensions string, caseSensitive bool) error {
	var p string
	p = strings.Replace(find, `\`, `\\`, -1)
	p = strings.Replace(p, ".", "\\.", -1)

	pattern := "(?i:.*(" + strings.ToLower(p) + ").*)"
	reg := regexp.MustCompile(pattern)
	ignores := splitToMap(strings.ToLower(ignoredirs), ",", "")
	extMap := splitToMap(textExtensions, ",", ".")

	var err error

	// do directories first. then we won't have to worry about stuff moving
	newpath, err := renameDirs(dir, replace, reg, ignores)

	if err != nil {
		return err
	}

	err = replaceContents(newpath, replace, reg, extMap, ignores)

	return err
}

type RenameOp struct {
	Old, New string
}

type ReadOp struct {
	Path     string
	Contents []byte
}

type WriteOp struct {
	Path     string
	Contents []byte
}

func renameDirs(dir, replace string, reg *regexp.Regexp, ignoreMap map[string]bool) (string, error) {
	renames := []RenameOp{} // do a list so they're processed in the correct order

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			fmt.Println(err)
			return err
		}

		lname := strings.ToLower(info.Name())

		if _, ok := ignoreMap[lname]; ok && info.IsDir() {
			return filepath.SkipDir
		}

		matches := reg.FindAllStringSubmatch(info.Name(), -1)
		if len(matches) == 0 {
			return nil
		}

		curdir := filepath.Dir(path)
		s := matches[0][1]

		newthisname := strings.Replace(info.Name(), s, replace, -1)
		renameTo := filepath.Join(curdir, newthisname)
		renames = append(renames, RenameOp{Old: path, New: renameTo})

		return nil
	})

	for i := len(renames) - 1; i >= 0; i-- {
		value := renames[i]
		err := os.Rename(value.Old, value.New)
		if err != nil {
			return dir, fmt.Errorf("Couldn't rename %v to %v, %s", value.Old, value.New, err)
		}
	}

	newpath := dir
	if len(renames) > 0 && renames[0].Old == dir {
		newpath = renames[0].New
	}

	return newpath, nil
}

func replaceContents(dir, replace string, reg *regexp.Regexp, extMap, ignoreMap map[string]bool) error {
	readPaths := []string{}

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			fmt.Println(err)
			return err
		}

		lname := strings.ToLower(info.Name())

		if _, ok := ignoreMap[lname]; ok && info.IsDir() {
			return filepath.SkipDir
		}

		if info.IsDir() {
			return nil
		}

		ext := filepath.Ext(strings.ToLower(info.Name()))

		if _, ok := extMap[ext]; !ok || len(ext) == 0 {
			return nil
		}

		readPaths = append(readPaths, path)

		return nil
	})

	reads := brokerRead(readPaths)
	writes := brokerUpdate(reads, reg, replace)
	brokerWrite(writes)

	return nil
}

func brokerRead(list []string) []ReadOp {
	readOps := make(chan ReadOp, len(list))
	if len(list) > GOPROCESSES*2 && GOPROCESSES > 1 {
		var wg sync.WaitGroup
		wg.Add(GOPROCESSES)
		groupSize := len(list)/GOPROCESSES + 1

		for i := 0; i < GOPROCESSES; i++ {
			grp := list[(i * groupSize) : (i+1)*groupSize]
			go func(lst []string) {
				ops := read(lst)
				for _, op := range ops {
					readOps <- op
				}
				wg.Done()
			}(grp)
		}

		wg.Wait()
		close(readOps)

		a := []ReadOp{}
		for r := range readOps {
			a = append(a, r)
		}

		return a
	} else { // just add all to first
		ops := read(list)
		return ops
	}
}

func read(list []string) []ReadOp {
	readOps := []ReadOp{}
	for _, path := range list {
		if path == "" {
			continue
		}
		bytes, err := os.ReadFile(path)
		if err != nil {
			fmt.Println("Got error reading file", path)
			continue
		}

		readOps = append(readOps, ReadOp{Path: path, Contents: bytes})
	}

	return readOps
}

func brokerUpdate(list []ReadOp, reg *regexp.Regexp, replace string) []WriteOp {
	writeOps := make(chan WriteOp, len(list))
	if len(list) > GOPROCESSES*2 && GOPROCESSES > 1 {
		var wg sync.WaitGroup
		wg.Add(GOPROCESSES)
		groupSize := len(list)/GOPROCESSES + 1

		for i := 0; i < GOPROCESSES; i++ {
			grp := list[(i * groupSize) : (i+1)*groupSize]
			go func(lst []ReadOp, reg *regexp.Regexp, replace string) {
				ops := update(lst, reg, replace)
				for _, op := range ops {
					writeOps <- op
				}
				wg.Done()
			}(grp, reg, replace)
		}

		wg.Wait()
		close(writeOps)

		a := []WriteOp{}
		for r := range writeOps {
			a = append(a, r)
		}

		return a
	} else { // just add all to first
		ops := update(list, reg, replace)
		return ops
	}
}

func update(list []ReadOp, reg *regexp.Regexp, replace string) []WriteOp {
	writes := []WriteOp{}
	for _, read := range list {
		matches := reg.FindAllSubmatch(read.Contents, 1)

		if len(matches) > 0 {
			f := string(matches[0][1])
			replaced := strings.Replace(string(read.Contents), f, replace, -1)
			write := WriteOp{Path: read.Path, Contents: []byte(replaced)}
			writes = append(writes, write)
		}
	}
	return writes
}

func brokerWrite(list []WriteOp) {
	if len(list) > GOPROCESSES*2 && GOPROCESSES > 1 {
		var wg sync.WaitGroup
		wg.Add(GOPROCESSES)
		groupSize := len(list)/GOPROCESSES + 1

		for i := 0; i < GOPROCESSES; i++ {
			grp := list[(i * groupSize) : (i+1)*groupSize]
			go func(lst []WriteOp) {
				write(lst)
				wg.Done()
			}(grp)
		}

		wg.Wait()
	} else {
		write(list)
	}
}

func write(list []WriteOp) {
	for _, wr := range list {
		var err error
		err = os.Remove(wr.Path)
		if err != nil {
			fmt.Println("Couldn't remove path", wr.Path, err)
		}

		err = os.WriteFile(wr.Path, wr.Contents, os.ModePerm)
		if err != nil {
			fmt.Println("Got error writing file", wr.Path, err)
		}
	}
}

func splitToMap(str, split, prefix string) map[string]bool {
	sp := strings.Split(str, split)
	m := make(map[string]bool, len(sp))
	for _, s := range sp {
		ls := strings.ToLower(s)
		ls = prefix + ls
		m[ls] = true
	}
	return m
}

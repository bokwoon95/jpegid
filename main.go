package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

// 2006-01-02T15.04.05.JPG

// TODO: use this instead https://pkg.go.dev/github.com/ncruces/go-exiftool#Server

var timestampFilenamePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}\.\d{2}\.\d{2}((?i)\.jpg|jpeg|png)`)

func main() {
	err := func() error {
		var dryRun bool
		flagset := flag.NewFlagSet("jpegid", flag.ContinueOnError)
		flagset.BoolVar(&dryRun, "dry-run", false, "")
		err := flagset.Parse(os.Args[1:])
		if err != nil {
			return err
		}
		flagArgs := flagset.Args()
		for _, flagArg := range flagArgs {
			dirEntries, err := fs.ReadDir(os.DirFS(flagArg), "/Users/bokwoon/Pictures")
			if err != nil {
				return err
			}
			for j, dirEntry := range dirEntries {
				if j > 5 {
					break
				}
				fileName := dirEntry.Name()
				filePath := filepath.ToSlash(filepath.Join(flagArg, fileName))
				if timestampFilenamePattern.MatchString(fileName) {
					fmt.Printf("skipping %s\n", filePath)
					continue
				}
				fmt.Printf("reading %s\n", filePath)
				b, err := exec.Command("exiftool", "-s3", "-CreateDate", filePath).Output()
				if err != nil {
					return err
				}
				creationDate, err := time.Parse("", string(b))
				if err != nil {
					return err
				}
				fmt.Printf("%s\n", creationDate.String())
			}
		}
		return nil
	}()
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Println(err)
		os.Exit(1)
	}
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// 2006-01-02T15.04.05.JPG
// 2006-01-02T15_04_05+08.01jjdz28.JPG
// 2006-01-02T150405.01jjdz28.JPG
// 01jjdz28.2006-01-02.15-04-05(08).JPG

func main() {
	userInterrupt := make(chan os.Signal, 1)
	signal.Notify(userInterrupt, syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-userInterrupt // Soft interrupt.
		cancel()
		<-userInterrupt // Hard interrupt.
		os.Exit(1)
	}()
	jpegIDCmd, err := JpegIDCommand(ctx, os.Args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}
	err = jpegIDCmd.Run()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

type Exif struct {
	FileSize           string
	DateTimeOriginal   string
	OffsetTimeOriginal string
}

type Task struct {
	completed chan struct{}
	filePath  string
	exif      Exif
	err       error
}

// jpegid -root . -file DSC -recursive -verbose -num-workers 8 -dry-run

// TODO: why would I multiplex 4 worker goroutines to 4 persistent stay_open exiftool commands? Just have each worker goroutine spin up its own stay_open exiftool invocation and continually feed data into it, parse the output, and execute the rename in-place. Each worker goroutine never has to return anything, since its output is just the rename (or simply printing the dry run results if -dry-run is enabled). You don't even need a Task struct anymore because you're not returning anything, just feeding a filePath into a channel and continuing.

// 2006-01-02.15-04-05(08).01jjdz28.JPG

type JpegIDCmd struct {
	Roots       []string
	FileRegexps []*regexp.Regexp
	NumWorkers  int
	Recursive   bool
	Verbose     bool
	DryRun      bool
	ExitOnError bool
	Stdout      io.Writer
	Stderr      io.Writer
	ctx         context.Context
}

func JpegIDCommand(ctx context.Context, args []string) (*JpegIDCmd, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	jpegIDCmd := &JpegIDCmd{
		Roots:  []string{cwd},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		ctx:    ctx,
	}
	flagset := flag.NewFlagSet("", flag.ContinueOnError)
	flagset.IntVar(&jpegIDCmd.NumWorkers, "num-workers", 8, "Number of concurrent workers.")
	flagset.BoolVar(&jpegIDCmd.Recursive, "recursive", false, "Walk the roots recursively.")
	flagset.BoolVar(&jpegIDCmd.Verbose, "verbose", false, "Verbose output.")
	flagset.BoolVar(&jpegIDCmd.DryRun, "dry-run", false, "Print rename operations without executing.")
	flagset.BoolVar(&jpegIDCmd.ExitOnError, "exit-on-error", false, "Exit on any error encountered.")
	flagset.Func("root", "Specify an additional root directory to watch. Can be repeated.", func(value string) error {
		root, err := filepath.Abs(value)
		if err != nil {
			return err
		}
		jpegIDCmd.Roots = append(jpegIDCmd.Roots, root)
		return nil
	})
	flagset.Func("file", "Include file regex. Can be repeated.", func(value string) error {
		r, err := compileRegexp(value)
		if err != nil {
			return err
		}
		jpegIDCmd.FileRegexps = append(jpegIDCmd.FileRegexps, r)
		return nil
	})
	err = flagset.Parse(args[1:])
	if err != nil {
		return nil, err
	}
	return jpegIDCmd, nil
}

func (jpegIDCmd *JpegIDCmd) Run() error {
	return nil
}

func compileRegexp(pattern string) (*regexp.Regexp, error) {
	n := strings.Count(pattern, ".")
	if n == 0 {
		return regexp.Compile(pattern)
	}
	if strings.HasPrefix(pattern, "./") && len(pattern) > 2 {
		pattern = pattern[2:]
	}
	var b strings.Builder
	b.Grow(len(pattern) + n)
	j := 0
	for j < len(pattern) {
		prev, _ := utf8.DecodeLastRuneInString(b.String())
		curr, width := utf8.DecodeRuneInString(pattern[j:])
		next, _ := utf8.DecodeRuneInString(pattern[j+width:])
		j += width
		if prev != '\\' && curr == '.' && (('a' <= next && next <= 'z') || ('A' <= next && next <= 'Z')) {
			b.WriteString("\\.")
		} else {
			b.WriteRune(curr)
		}
	}
	return regexp.Compile(b.String())
}

func main_Old() {
	var timestampFilenamePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}\.\d{2}\.\d{2}((?i)\.jpg|jpeg|png)`)
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

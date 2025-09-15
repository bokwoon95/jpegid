package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
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
	jpegIDCmd, err := JpegIDCommand(os.Args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}
	err = jpegIDCmd.Run(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Println(err)
			os.Exit(1)
		}
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
	logger      *slog.Logger
}

func JpegIDCommand(args []string) (*JpegIDCmd, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	jpegIDCmd := &JpegIDCmd{
		Roots:  []string{cwd},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
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
	handlerOptions := &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelError,
	}
	if jpegIDCmd.Verbose {
		handlerOptions.Level = slog.LevelInfo
	}
	jpegIDCmd.logger = slog.New(slog.NewTextHandler(jpegIDCmd.Stdout, handlerOptions))
	return jpegIDCmd, nil
}

func (jpegIDCmd *JpegIDCmd) Run(ctx context.Context) error {
	filePaths := make(chan string)
	for i := 0; i < jpegIDCmd.NumWorkers; i++ {
		err := jpegIDCmd.startWorker(ctx, filePaths)
		if err != nil {
			return err
		}
	}
	for _, root := range jpegIDCmd.Roots {
		if jpegIDCmd.Recursive {
			err := fs.WalkDir(os.DirFS(root), ".", func(path string, dirEntry fs.DirEntry, err error) error {
				jpegIDCmd.logger.Info("walking", slog.String("path", path))
				if err != nil {
					return err
				}
				if dirEntry.IsDir() {
					return nil
				}
				name := dirEntry.Name()
				for _, fileRegexp := range jpegIDCmd.FileRegexps {
					if fileRegexp.MatchString(name) {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case filePaths <- filepath.Join(root, path):
							break
						}
						return nil
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		} else {
			jpegIDCmd.logger.Info("readdir", slog.String("root", root))
			dirEntries, err := fs.ReadDir(os.DirFS(root), ".")
			if err != nil {
				return err
			}
			for _, dirEntry := range dirEntries {
				if dirEntry.IsDir() {
					continue
				}
				name := dirEntry.Name()
				for _, fileRegexp := range jpegIDCmd.FileRegexps {
					if fileRegexp.MatchString(name) {
						select {
						case <-ctx.Done():
							return nil
						case filePaths <- filepath.Join(root, name):
							break
						}
					}
				}
			}
		}
	}
	return nil
}

func (jpegIDCmd *JpegIDCmd) startWorker(ctx context.Context, filePaths <-chan string) error {
	type Exif struct {
		FileSize           string
		DateTimeOriginal   string
		OffsetTimeOriginal string
	}
	exifToolCmd := exec.Command("exiftool", "-stay_open", "True", "-@", "-")
	setpgid(exifToolCmd)
	exifToolStdin, err := exifToolCmd.StdinPipe()
	if err != nil {
		return err
	}
	exifToolStdout, err := exifToolCmd.StdoutPipe()
	if err != nil {
		return err
	}
	exifToolStderr, err := exifToolCmd.StderrPipe()
	if err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(jpegIDCmd.Stderr, exifToolStderr)
	}()
	err = exifToolCmd.Start()
	if err != nil {
		return fmt.Errorf("starting %s: %w", exifToolCmd.String(), err)
	}
	defer func() {
		_, err := io.WriteString(exifToolStdin, "-stay_open\n"+
			"False\n")
		if err != nil {
			jpegIDCmd.logger.Warn(err.Error())
		}
		stop(exifToolCmd)
	}()
	var filePath string
	var buf bytes.Buffer
	reader := bufio.NewReader(exifToolStdout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case filePath = <-filePaths:
			break
		}
		_, err := io.WriteString(exifToolStdin, "-json\n"+
			filePath+"\n"+
			"-execute\n")
		if err != nil {
			return fmt.Errorf("executing -json for %s: %w", filePath, err)
		}
		buf.Reset()
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if err == io.EOF {
					return fmt.Errorf("exiftool returned EOF prematurely")
				}
				return fmt.Errorf("exiftool response: %w", err)
			}
			if string(line) != "{ready}\n" {
				buf.Write(line)
				continue
			}
			break
		}
		var exifs []Exif
		err = json.Unmarshal(buf.Bytes(), &exifs)
		if err != nil {
			return fmt.Errorf("unmarshaling %s: %w", buf.String(), err)
		}
		exif := exifs[0]
		fmt.Fprintf(jpegIDCmd.Stdout, "%s %s %s %s", filePath, exif.FileSize, exif.DateTimeOriginal, exif.OffsetTimeOriginal)
	}
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

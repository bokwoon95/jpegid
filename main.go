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
	"sync"
	"syscall"
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
	cmd, err := JpegIDCommand(os.Args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}
	err = cmd.Run(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Println(err)
			os.Exit(1)
		}
	}
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
	jpegidCmd := &JpegIDCmd{
		Roots:  []string{cwd},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	flagset := flag.NewFlagSet("", flag.ContinueOnError)
	flagset.IntVar(&jpegidCmd.NumWorkers, "num-workers", 8, "Number of concurrent workers.")
	flagset.BoolVar(&jpegidCmd.Recursive, "recursive", false, "Walk the roots recursively.")
	flagset.BoolVar(&jpegidCmd.Verbose, "verbose", false, "Verbose output.")
	flagset.BoolVar(&jpegidCmd.DryRun, "dry-run", false, "Print rename operations without executing.")
	flagset.BoolVar(&jpegidCmd.ExitOnError, "exit-on-error", false, "Exit on any error encountered.")
	flagset.Func("root", "Specify an additional root directory to watch. Can be repeated.", func(value string) error {
		root, err := filepath.Abs(value)
		if err != nil {
			return err
		}
		jpegidCmd.Roots = append(jpegidCmd.Roots, root)
		return nil
	})
	flagset.Func("file", "Include file regex. Can be repeated.", func(value string) error {
		r, err := compileRegexp(value)
		if err != nil {
			return err
		}
		jpegidCmd.FileRegexps = append(jpegidCmd.FileRegexps, r)
		return nil
	})
	err = flagset.Parse(args[1:])
	if err != nil {
		return nil, err
	}
	logLevel := slog.LevelError
	if jpegidCmd.Verbose {
		logLevel = slog.LevelInfo
	}
	jpegidCmd.logger = slog.New(slog.NewTextHandler(jpegidCmd.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     logLevel,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			switch attr.Key {
			case slog.TimeKey:
				return slog.Attr{}
			case slog.SourceKey:
				source := attr.Value.Any().(*slog.Source)
				return slog.Any(slog.SourceKey, &slog.Source{
					Function: source.Function,
					File:     filepath.Base(source.File),
					Line:     source.Line,
				})
			default:
				return attr
			}
		},
	}))
	return jpegidCmd, nil
}

func (jpegidCmd *JpegIDCmd) Run(ctx context.Context) error {
	type Exif struct {
		FileSize           string
		DateTimeOriginal   string
		OffsetTimeOriginal string
	}
	jpegidCmd.logger.Info("running", slog.String("whee", "gee"))
	var waitGroup sync.WaitGroup
	defer waitGroup.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	filePaths := make(chan string)
	for i := 0; i < jpegidCmd.NumWorkers; i++ {
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
			_, _ = io.Copy(jpegidCmd.Stderr, exifToolStderr)
		}()
		err = exifToolCmd.Start()
		if err != nil {
			return fmt.Errorf("starting %s: %w", exifToolCmd.String(), err)
		}
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			defer func() {
				_, err := io.WriteString(exifToolStdin, "-stay_open\n"+
					"False\n")
				if err != nil {
					jpegidCmd.logger.Warn(err.Error())
				}
				stop(exifToolCmd)
			}()
			var buf bytes.Buffer
			reader := bufio.NewReader(exifToolStdout)
			for {
				select {
				case <-ctx.Done():
					return
				case filePath := <-filePaths:
					_, err := io.WriteString(exifToolStdin, "-json\n"+
						filePath+"\n"+
						"-execute\n")
					if err != nil {
						jpegidCmd.logger.Error(err.Error(), slog.String("filePath", filePath))
						return
					}
					buf.Reset()
					for {
						line, err := reader.ReadBytes('\n')
						if err != nil {
							if err == io.EOF {
								jpegidCmd.logger.Error("exiftool returned EOF prematurely")
								return
							}
							jpegidCmd.logger.Error(err.Error())
							return
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
						jpegidCmd.logger.Error(err.Error(), slog.String("data", buf.String()))
						return
					}
					exif := exifs[0]
					fmt.Fprintf(jpegidCmd.Stdout, "%s %s %s %s\n", filePath, exif.FileSize, exif.DateTimeOriginal, exif.OffsetTimeOriginal)
				}
			}
		}()
	}
	for _, root := range jpegidCmd.Roots {
		if jpegidCmd.Recursive {
			err := fs.WalkDir(os.DirFS(root), ".", func(path string, dirEntry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if dirEntry.IsDir() {
					return nil
				}
				name := dirEntry.Name()
				for _, fileRegexp := range jpegidCmd.FileRegexps {
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
			dirEntries, err := fs.ReadDir(os.DirFS(root), ".")
			if err != nil {
				return err
			}
			for _, dirEntry := range dirEntries {
				if dirEntry.IsDir() {
					continue
				}
				name := dirEntry.Name()
				for _, fileRegexp := range jpegidCmd.FileRegexps {
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

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

// Rerun defines a command to rerun
type Rerun struct {
	sync.WaitGroup
	Command string
	exiting bool
	cancel  context.CancelFunc
	watcher *fsnotify.Watcher
}

// Start runs the command in a go routine
func (r *Rerun) Start() {
	log.Debug("Called Start()")

	// Create context with a cancel function
	var ctx context.Context
	ctx, r.cancel = context.WithCancel(context.Background())

	// Make sure we're not exiting
	if !r.exiting {
		// Start execution of the provided command
		r.Add(1)
		go func() {
			log.Debug("Started go routine for new command execution")
			defer r.Done()
			// Context is used to kill the running command from outside the go routine
			cmd := exec.CommandContext(ctx, "sh", "-c", r.Command)

			// Immediately write out all stdout and stderr from the running command
			var stdoutBuf, stderrBuf bytes.Buffer
			cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)
			cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
			cmd.Start()
			log.Debugf("Command is running: %q", r.Command)
			// Start for loop waiting for the cancel function to be called
			for {
				select {
				case <-ctx.Done():
					log.Debug("Command has stoped and the go routine is closing")
					return // returning to not leak the goroutine
				}
			}
		}()
	}
}

// Stop runs the command in a go routine
func (r *Rerun) Stop() {
	log.Debug("Called Stop()")
	r.cancel()
	// Wait until go routine has ended before continuing
	log.Debug("Waiting for waitgroup to be empty")
	r.Wait()
}

// WatchDir implements filepath.WalkFunc and adds paths to the filesystem watcher
func (r *Rerun) WatchDir(path string, f os.FileInfo, err error) error {
	if f.IsDir() {
		// Ignore .git directory since it's noisy
		if f.Name() == ".git" {
			log.Debug("Ignoring .git directory")
			return filepath.SkipDir
		}
		// Add directory to the list of directories to watch
		err = r.watcher.Add(path)
		if err != nil {
			log.Debugf("Unable to watch directory %q", path)
		} else {
			log.Debugf("Added %q directory to filesystem watcher", path)
		}
	}
	return err
}

// UnwatchDir removes paths from the filesystem watcher
func (r *Rerun) UnwatchDir(path string) {
	err := r.watcher.Remove(path)
	if err == nil {
		log.Debugf("Removed %q directory from filesystem watcher", path)
	}
}

// Events returns a channel from filesystem watcher
func (r *Rerun) Events() chan fsnotify.Event {
	return r.watcher.Events
}

// NewRerun returns a configured rerun
func NewRerun(command string) *Rerun {
	log.Debug("Called NewRerun()")
	var err error
	var rerun Rerun
	rerun.exiting = false
	rerun.Command = command

	// Setup a filesystem watcher to detect new files, directories, and changes
	rerun.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Filesystem watcher error: %q", err)
	}

	// Get current directory
	curDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Unable to determine current directory: %q", err)
	}

	log.Debug("Finding sub directories to watch for changes")
	// Walk through file system to watch sub directories
	err = filepath.Walk(curDir, rerun.WatchDir)

	// Catch ctrl+c and kill the current running command cleanly
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		rerun.cleanup()
		os.Exit(1)
	}()

	return &rerun
}

// cleanup will stop a running command, wait for waitgroups to close and stop
// the filesystem watcher
func (r *Rerun) cleanup() {
	log.Debug("Called cleanup()")
	// TODO: We should use a mutex for locking instead of this exiting bool
	r.exiting = true
	r.Stop()
	log.Debug("Stopping the filesystem watcher")
	r.watcher.Close()
}

func main() {
	// Get args
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println(errors.New("You must provide a command to run"))
		os.Exit(1)
	}

	// Check for debug flag
	if args[0] == "--debug" {
		args = args[1:]
		if len(args) == 0 {
			fmt.Println(errors.New("You must provide a command to run"))
			os.Exit(1)
		}
		log.SetReportCaller(true)
		log.SetLevel(log.DebugLevel)
	}

	// Initialize rerun command
	run := NewRerun(strings.Join(args, " "))
	defer run.cleanup()

	// Start initial execution of the provided command
	run.Start()

	log.Debug("Starting main loop")
	for {
		select {
		case event := <-run.Events():
			log.Debug("Filesystem watcher received an event")
			log.Debug("File system event: " + event.String())

			// Add new directories to watch list
			if event.Op&fsnotify.Create == fsnotify.Create {
				fileInfo, err := os.Stat(event.Name)
				if err != nil {
					log.Errorf("Unable to get filesystem info about %q", event.Name)
				} else {
					run.WatchDir(event.Name, fileInfo, nil)
				}
			}

			// Try to remove deleted directories from the watch list
			if event.Op&fsnotify.Remove == fsnotify.Remove {
				run.UnwatchDir(event.Name)
			}

			// Kill current running command
			run.Stop()
			// Start new execution of the provided command
			run.Start()
		}
	}
}

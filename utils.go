package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/fatih/color"
	"github.com/pkg/errors"
)

func checkf(err error, format string, args ...interface{}) {
	if err != nil {
		log.Printf(format, args...)
		log.Println()
		log.Fatalf("%+v", errors.WithStack(err))
	}
}

func assertf(ok bool, format string, args ...interface{}) {
	if !ok {
		log.Printf(format, args...)
		log.Println()
		log.Fatalf("%+v", errors.Errorf("Should be true, but is false"))
	}
}

var errc = color.New(color.BgRed, color.FgWhite).PrintfFunc()

func oerr(msg string) {
	errc("\tERROR: " + msg + " ")
	fmt.Println()
	fmt.Println("Flags available:")
	flag.PrintDefaults()
	fmt.Println()
}

/// runCommand excute the given cmd and return the list of lines outputed on
/// stdout
func runCommand(name string, arg ...string) []string {
	cmd := exec.Command(name, arg...)
	out, err := cmd.Output()
	checkf(err, "Error running `%v`: `%v`", name, arg)
	return strings.Split(string(out), "\n")
}

// Parameters for Fzf
type Fzf struct {
	// Items to be selected by user
	Items []string
	// If prompt is set "", a default prompt is used.
	Prompt string
	//  Used to fill the search field
	Query string
	// Returns what the user searched for
	ReturnQuery bool
	// More arguments to the fzf command
	MoreArgs []string
}

// FuzzySelect prompts the user to select one or more items in a fuzzy menu.
func fuzzySelect(f Fzf) (selected []string) {
	// TODO Find a more idiomatic way
	args := []string{}
	if f.Prompt != "" {
		args = append(args, "--prompt", f.Prompt+" >")
	}
	if f.ReturnQuery {
		// Print what the user entered first-line
		args = append(args, "--print-query")
	}
	args = append(args, "--query", f.Query)
	// Default options, TODO Make this configurable
	args = append(args, "--height", "80%", "--layout=reverse")
	// Other options
	args = append(args, f.MoreArgs...)
	// Inspired from https://stackoverflow.com/a/23167416/ by mraron (Apache
	// 2.0 licence)
	subProcess := exec.Command("fzf", args...)
	stdin, err := subProcess.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
	stdout, err := subProcess.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	subProcess.Stderr = os.Stderr
	err = subProcess.Start()
	checkf(err, "Error running fzf. Is https://github.com/junegunn/fzf installed?")
	io.WriteString(stdin, strings.Join(f.Items, "\n"))
	buf := new(bytes.Buffer)
	buf.ReadFrom(stdout)
	s := buf.String()
	subProcess.Wait()
	for i, s := range strings.Split(s, "\n") {
		switch {
		// Always keep user query even if it is empty
		case f.ReturnQuery && i == 0:
			selected = append(selected, s)
		case s != "":
			selected = append(selected, s)
		}
	}
	return
}

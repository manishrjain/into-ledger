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

/// FuzzySelect prompts the user to select one or more items in a fuzzy menu.
func fuzzySelect(items []string) (selected []string) {
	// Inspired from https://stackoverflow.com/a/23167416/ by mraron (Apache
	// 2.0 licence)
	subProcess := exec.Command("fzf")
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
	io.WriteString(stdin, strings.Join(items, "\n"))
	buf := new(bytes.Buffer)
	buf.ReadFrom(stdout)
	s := buf.String()
	subProcess.Wait()
	for _, s := range strings.Split(s, "\n") {
		if s != "" {
			selected = append(selected, s)
		}
	}
	return
}

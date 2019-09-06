package main

import (
	"flag"
	"fmt"
	"log"
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

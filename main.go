package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"

	"github.com/pkg/errors"
)

var (
	journal = flag.String("j", "", "Existing journal to learn from.")
	rtxn    = regexp.MustCompile(`\d{4}/\d{2}/\d{2}[\W]*(\w.*)`)
	rto     = regexp.MustCompile(`\W*([:\w]+).*`)
	rfrom   = regexp.MustCompile(`\W*([:\w]+).*`)
)

func assert(err error) {
	if err != nil {
		log.Fatal(errors.WithStack(err))
	}
}

func check(ok bool) {
	if !ok {
		log.Fatal(errors.Errorf("Should be true, but is false"))
	}
}

func parseTransaction(s *bufio.Scanner) bool {
	for s.Scan() {
		m := rtxn.FindStringSubmatch(s.Text())
		if len(m) < 2 {
			continue
		}
		desc := m[1]

		check(s.Scan())
		m = rto.FindStringSubmatch(s.Text())
		check(len(m) > 1)
		to := m[1]

		check(s.Scan())
		m = rfrom.FindStringSubmatch(s.Text())
		check(len(m) > 1)
		from := m[1]
		fmt.Printf("[%v] [%v] [%v]\n", desc, to, from)
		return true
	}
	return false
}

func main() {
	flag.Parse()
	f, err := os.Open(*journal)
	assert(err)
	s := bufio.NewScanner(f)
	for parseTransaction(s) {
	}
}

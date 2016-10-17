package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jbrukh/bayesian"
	"github.com/pkg/errors"
)

var (
	journal  = flag.String("j", "", "Existing journal to learn from.")
	debug    = flag.Bool("debug", false, "Additional debug information if set.")
	csv      = flag.String("csv", "", "File path of CSV file containing new transactions.")
	account  = flag.String("a", "", "Name of bank account to use.")
	currency = flag.String("c", "", "Set currency if any.")
	ignore   = flag.String("ic", "", "Comma separated list of colums to ignore in CSV.")
	rtxn     = regexp.MustCompile(`\d{4}/\d{2}/\d{2}[\W]*(\w.*)`)
	rto      = regexp.MustCompile(`\W*([:\w]+).*`)
	rfrom    = regexp.MustCompile(`\W*([:\w]+).*`)
	racc     = regexp.MustCompile(`^account[\W]+(.*)`)
	ralias   = regexp.MustCompile(`\balias\s(.*)`)
	stamp    = "2006/01/02"
)

func assert(err error) {
	if err != nil {
		log.Fatalf("%+v", errors.WithStack(err))
	}
}

func check(ok bool) {
	if !ok {
		log.Fatalf("%+v", errors.Errorf("Should be true, but is false"))
	}
}

type txn struct {
	date string
	desc string
	to   string
	from string
	cur  float64
}

type parser struct {
	data     []byte
	txns     []txn
	classes  []bayesian.Class
	cl       *bayesian.Classifier
	accounts map[string]string
}

func (p *parser) parseTransactions() {
	s := bufio.NewScanner(bytes.NewReader(p.data))
	var t txn
	for s.Scan() {
		t = txn{}
		m := rtxn.FindStringSubmatch(s.Text())
		if len(m) < 2 {
			continue
		}
		t.desc = strings.ToLower(m[1])

		check(s.Scan())
		m = rto.FindStringSubmatch(s.Text())
		check(len(m) > 1)
		t.to = m[1]
		if alias, has := p.accounts[t.to]; has {
			fmt.Printf("%s -> %s\n", t.to, alias)
			t.to = alias
		}

		check(s.Scan())
		m = rfrom.FindStringSubmatch(s.Text())
		check(len(m) > 1)
		t.from = m[1]
		p.txns = append(p.txns, t)
	}
}

func (p *parser) parseAccounts() {
	p.accounts = make(map[string]string)
	s := bufio.NewScanner(bytes.NewReader(p.data))
	for s.Scan() {
		m := racc.FindStringSubmatch(s.Text())
		if len(m) < 2 {
			continue
		}
		acc := m[1]

		check(s.Scan())
		m = ralias.FindStringSubmatch(s.Text())
		check(len(m) > 1)
		ali := m[1]
		p.accounts[acc] = ali
	}
}

func (p *parser) generateClasses() {
	p.classes = make([]bayesian.Class, 0, 10)
	tomap := make(map[string]bool)
	for _, t := range p.txns {
		tomap[t.to] = true
	}
	for to := range tomap {
		p.classes = append(p.classes, bayesian.Class(to))
	}

	p.cl = bayesian.NewClassifierTfIdf(p.classes...)
	for _, t := range p.txns {
		fmt.Println(strings.Split(t.desc, " "), bayesian.Class(t.to))
		p.cl.Learn(strings.Split(t.desc, " "), bayesian.Class(t.to))
	}
	p.cl.ConvertTermsFreqToTfIdf()
}

type pair struct {
	score float64
	pos   int
}

type byScore []pair

func (b byScore) Len() int {
	return len(b)
}
func (b byScore) Less(i int, j int) bool {
	return b[i].score > b[j].score
}
func (b byScore) Swap(i int, j int) {
	b[i], b[j] = b[j], b[i]
}

func (p *parser) topHits(in string) []bayesian.Class {
	in = strings.ToLower(in)
	terms := strings.Split(in, " ")
	scores, _, _ := p.cl.LogScores(terms)
	pairs := make([]pair, 0, len(scores))

	var mean, stddev float64
	for pos, score := range scores {
		pairs = append(pairs, pair{score, pos})
		mean += score
	}
	mean /= float64(len(scores))
	for _, score := range scores {
		stddev += math.Pow(score-mean, 2)
	}
	stddev /= float64(len(scores) - 1)
	stddev = math.Sqrt(stddev)
	fmt.Println(len(terms), terms, stddev)

	sort.Sort(byScore(pairs))
	result := make([]bayesian.Class, 0, 5)
	last := pairs[0].score
	for i := 0; i < 5; i++ {
		pr := pairs[i]
		if *debug {
			fmt.Printf("i=%d s=%f Class=%v\n", i, pr.score, p.classes[pr.pos])
		}
		if math.Abs(pr.score-last) > stddev {
			break
		}
		result = append(result, p.classes[pr.pos])
		last = pr.score
	}
	return result
}

func includeAll(dir string, data []byte) []byte {
	final := make([]byte, len(data))
	copy(final, data)

	b := bytes.NewBuffer(data)
	s := bufio.NewScanner(b)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "include ") {
			continue
		}
		fname := strings.Trim(line[8:], " \n")
		include, err := ioutil.ReadFile(path.Join(dir, fname))
		assert(err)
		final = append(final, include...)
	}
	return final
}

func parseDate(col string) (string, bool) {
	if tm, err := time.Parse("01/02/2006", col); err == nil {
		return tm.Format(stamp), true
	}
	if tm, err := time.Parse("02/01/2006", col); err == nil {
		return tm.Format(stamp), true
	}
	return "", false
}

func parseCurrency(col string) (float64, bool) {
	f, err := strconv.ParseFloat(col, 64)
	return f, err == nil
}

func parseDescription(col string) (string, bool) {
	if len(col) < 2 {
		return "", false
	}
	if col[0] != '"' || col[len(col)-1] != '"' {
		return "", false
	}
	return col[1 : len(col)-1], true
}

func parseTransactionsFromCSV(in []byte) []txn {
	s := bufio.NewScanner(bytes.NewReader(in))
	var t txn
	ignored := make(map[int]bool)
	for _, i := range strings.Split(*ignore, ",") {
		pos, err := strconv.Atoi(i)
		assert(err)
		ignored[pos] = true
	}
	result := make([]txn, 0, 100)
	for s.Scan() {
		t = txn{}
		line := s.Text()
		if len(line) == 0 {
			continue
		}
		cols := strings.Split(line, ",")
		for i, col := range cols {
			if ignored[i] {
				continue
			}
			if date, ok := parseDate(col); ok {
				t.date = date
			}
			if f, ok := parseCurrency(col); ok {
				t.cur = f
			}
			if d, ok := parseDescription(col); ok {
				t.desc = d
			}
		}
		if len(t.desc) != 0 && len(t.date) != 0 && t.cur != 0.0 {
			result = append(result, t)
		} else {
			log.Fatalf("Unable to parse txn for [%v]. Got: %+v\n", line, t)
		}
	}
	return result
}

func main() {
	flag.Parse()
	data, err := ioutil.ReadFile(*journal)
	assert(err)
	alldata := includeAll(path.Dir(*journal), data)

	p := parser{data: alldata}
	p.parseAccounts()
	p.parseTransactions()

	// Scanning done. Now train classifier.
	p.generateClasses()

	in, err := ioutil.ReadFile(*csv)
	assert(err)
	txns := parseTransactionsFromCSV(in)
	for _, t := range txns {
		fmt.Printf("%+v\n", t)
	}
	return

	r := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Input: ")
		in, err := r.ReadString('\n')
		assert(err)
		in = strings.Trim(in, "\n")
		if len(in) == 0 {
			break
		}
		hits := p.topHits(in)
		_ = hits
	}
}

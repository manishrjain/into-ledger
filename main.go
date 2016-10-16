package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/jbrukh/bayesian"
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

type txn struct {
	desc string
	to   string
	from string
}

type parser struct {
	s       *bufio.Scanner
	txns    []txn
	classes []bayesian.Class
	cl      *bayesian.Classifier
}

func (p *parser) parseTransaction() bool {
	s := p.s
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

		check(s.Scan())
		m = rfrom.FindStringSubmatch(s.Text())
		check(len(m) > 1)
		t.from = m[1]
		p.txns = append(p.txns, t)
		return true
	}
	return false
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

func (p *parser) top3Hits(in string) {
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
	last := pairs[0].score
	for i := 0; i < 5; i++ {
		pr := pairs[i]
		fmt.Printf("i=%d s=%f Class=%v\n", i, pr.score, p.classes[pr.pos])
		if math.Abs(pr.score-last) > stddev {
			break
		}
		last = pr.score
	}
}

func main() {
	flag.Parse()
	f, err := os.Open(*journal)
	assert(err)
	p := parser{s: bufio.NewScanner(f)}
	for p.parseTransaction() {
	}

	p.generateClasses()

	r := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Input: ")
		in, err := r.ReadString('\n')
		assert(err)
		in = strings.Trim(in, "\n")
		if len(in) == 0 {
			break
		}
		p.top3Hits(in)
	}
}

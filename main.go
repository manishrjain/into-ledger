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
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
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

func assignFor(opt string, cl bayesian.Class, keys map[rune]string) bool {
	for i := 0; i < len(opt); i++ {
		ch := rune(opt[i])
		if _, has := keys[ch]; !has {
			keys[ch] = string(cl)
			return true
		}
	}
	return false
}

func generateKeyMap(opts []bayesian.Class) map[rune]string {
	keys := make(map[rune]string)
	keys['b'] = ".back"
	keys['q'] = ".quit"
	for _, opt := range opts {
		if ok := assignFor(string(opt), opt, keys); ok {
			continue
		}
		if ok := assignFor(strings.ToUpper(string(opt)), opt, keys); ok {
			continue
		}
		if ok := assignFor("0123456789", opt, keys); ok {
			continue
		}
		log.Fatalf("Unable to assign any key for: %v", opt)
	}
	return keys
}

type kv struct {
	key rune
	val string
}

type byVal []kv

func (b byVal) Len() int {
	return len(b)
}

func (b byVal) Less(i int, j int) bool {
	return b[i].val < b[j].val
}

func (b byVal) Swap(i int, j int) {
	b[i], b[j] = b[j], b[i]
}

var allKeys map[rune]string

func singleCharMode() {
	// disable input buffering
	exec.Command("stty", "-F", "/dev/tty", "cbreak", "min", "1").Run()
	// do not display entered characters on the screen
	exec.Command("stty", "-F", "/dev/tty", "-echo").Run()
}

func printSummary(t txn, idx, total int) {
	color.New(color.BgBlue, color.FgWhite).Printf(" [%2d of %2d] ", idx, total)
	color.New(color.BgYellow, color.FgBlack).Printf(" %10s ", t.date)
	desc := t.desc
	if len(desc) > 40 {
		desc = desc[:40]
	}
	color.New(color.BgWhite, color.FgBlack).Printf(" %-40s", desc)
	if len(t.to) > 0 {
		to := t.to
		if len(to) > 20 {
			to = to[len(to)-20:]
		}
		color.New(color.BgGreen, color.FgBlack).Printf(" %-20s ", to)
	}

	pomo := color.New(color.BgBlack, color.FgWhite).PrintfFunc()
	if t.cur > 0 {
		pomo = color.New(color.BgGreen, color.FgBlack).PrintfFunc()
	} else {
		pomo = color.New(color.BgRed, color.FgWhite).PrintfFunc()
	}
	pomo(" %7.2f ", t.cur)
	fmt.Println()
}

func clear() {
	cmd := exec.Command("clear")
	cmd.Stdout = os.Stdout
	cmd.Run()
	fmt.Println()
}

func printAndGetResult(keys map[rune]string, t *txn) int {
	kvs := make([]kv, 0, len(keys))
	for k, opt := range keys {
		kvs = append(kvs, kv{k, opt})
	}
	sort.Sort(byVal(kvs))
	for _, kv := range kvs {
		fmt.Printf("%q: %s\n", kv.key, kv.val)
	}

	r := make([]byte, 1)
	os.Stdin.Read(r)
	ch := rune(r[0])
	if ch == 'b' {
		return -1
	}
	if ch == 'q' {
		return 9999
	}
	if ch == rune(10) && len(t.to) > 0 {
		return 1
	}

	if opt, has := keys[ch]; has {
		t.to = opt
		return 1
	}
	return 0
}

func (p *parser) printTxn(t *txn, idx, total int) int {
	clear()
	printSummary(*t, idx, total)

	hits := p.topHits(t.desc)
	keys := generateKeyMap(hits)
	res := printAndGetResult(keys, t)
	if res != 0 {
		return res
	}

	clear()
	printSummary(*t, idx, total)
	return printAndGetResult(allKeys, t)
}

func (p *parser) showAndCategorizeTxns(txns []txn) {
	allKeys = generateKeyMap(p.classes)
	for i, t := range txns {
		printSummary(t, i, len(txns))
	}
	fmt.Println()

	fmt.Printf("Found %d transactions. Review (Y/n)? ", len(txns))
	b := make([]byte, 1)
	os.Stdin.Read(b)
	if b[0] == 'n' || b[0] == 'q' {
		return
	}

	for i := 0; i < len(txns) && i >= 0; {
		i += p.printTxn(&txns[i], i, len(txns))
	}
	for i, t := range txns {
		printSummary(t, i, len(txns))
	}
	fmt.Println()
}

func main() {
	singleCharMode()
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

	p.showAndCategorizeTxns(txns)
}

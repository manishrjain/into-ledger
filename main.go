package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/gob"
	"flag"
	"fmt"
	"io"
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

	yaml "gopkg.in/yaml.v2"

	"github.com/boltdb/bolt"
	"github.com/fatih/color"
	"github.com/jbrukh/bayesian"
	"github.com/manishrjain/keys"
	"github.com/pkg/errors"
)

var (
	journal    = flag.String("j", "", "Existing journal to learn from.")
	output     = flag.String("o", "out.ldg", "Journal file to write to.")
	debug      = flag.Bool("debug", false, "Additional debug information if set.")
	csvFile    = flag.String("csv", "", "File path of CSV file containing new transactions.")
	account    = flag.String("a", "", "Name of bank account transactions belong to.")
	currency   = flag.String("c", "$", "Set currency if any.")
	ignore     = flag.String("ic", "", "Comma separated list of columns to ignore in CSV.")
	dateFormat = flag.String("d", "01/02/2006", "Defaults to MM/DD/YYYY. "+
		"Express your date format w.r.t. Jan 02, 2006. See: https://golang.org/pkg/time/")
	configDir = flag.String("conf", os.Getenv("HOME")+"/.into-ledger",
		"Config directory to store various into-ledger configs in.")

	rtxn   = regexp.MustCompile(`(\d{4}/\d{2}/\d{2})[\W]*(\w.*)`)
	rto    = regexp.MustCompile(`\W*([:\w]+)(.*)`)
	rfrom  = regexp.MustCompile(`\W*([:\w]+).*`)
	rcur   = regexp.MustCompile(`(\d+\.\d+|\d+)`)
	racc   = regexp.MustCompile(`^account[\W]+(.*)`)
	ralias = regexp.MustCompile(`\balias\s(.*)`)

	stamp      = "2006/01/02"
	bucketName = []byte("txns")
	descLength = 40
	short      *keys.Shortcuts
)

type accountConfig struct {
	Currency   string
	Journal    string
	DateFormat string
	Ignore     string
	Output     string
}

type configs struct {
	Accounts map[string]accountConfig // account and the corresponding config.
}

type txn struct {
	Date               time.Time
	Desc               string
	To                 string
	From               string
	Cur                float64
	CurName            string
	Key                []byte
	skipClassification bool
}

type byTime []txn

func (b byTime) Len() int               { return len(b) }
func (b byTime) Less(i int, j int) bool { return !b[i].Date.After(b[j].Date) }
func (b byTime) Swap(i int, j int)      { b[i], b[j] = b[j], b[i] }

func check(err error, format string, args ...interface{}) {
	if err != nil {
		log.Printf(format, args)
		log.Fatalf("%+v", errors.WithStack(err))
	}
}

func assert(ok bool) {
	if !ok {
		log.Fatalf("%+v", errors.Errorf("Should be true, but is false"))
	}
}

type parser struct {
	db       *bolt.DB
	data     []byte
	txns     []txn
	classes  []bayesian.Class
	cl       *bayesian.Classifier
	accounts map[string]string
}

func (p *parser) parseTransactions(journal string) {
	out, err := exec.Command("ledger", "-f", journal, "csv").Output()
	check(err, "Unable to convert journal to csv.")
	r := csv.NewReader(bytes.NewReader(out))
	var t txn
	for {
		cols, err := r.Read()
		if err == io.EOF {
			break
		}
		check(err, "Unable to read a csv line.")

		t = txn{}
		t.Date, err = time.Parse(stamp, cols[0])
		check(err, "Unable to parse time: %v", cols[0])
		t.Desc = strings.Trim(cols[2], " \n\t")

		t.To = cols[3]
		assert(len(t.To) > 0)
		if strings.HasPrefix(t.To, "Assets:Reimbursements:") {
			// pass
		} else if strings.HasPrefix(t.To, "Assets:") {
			// Don't pick up Assets.
			t.skipClassification = true
		} else if strings.HasPrefix(t.To, "Equity:") {
			// Don't pick up Equity.
			t.skipClassification = true
		}
		if alias, has := p.accounts[t.To]; has {
			t.To = alias
		}
		t.CurName = cols[4]
		t.Cur, err = strconv.ParseFloat(cols[5], 64)
		check(err, "Unable to parse amount.")
		p.txns = append(p.txns, t)

		short.AutoAssign(t.To, "default")
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

		assert(s.Scan())
		m = ralias.FindStringSubmatch(s.Text())
		assert(len(m) > 1)
		ali := m[1]
		p.accounts[acc] = ali
	}

	for _, alias := range p.accounts {
		short.AutoAssign(alias, "default")
	}
}

func (p *parser) generateClasses() {
	p.classes = make([]bayesian.Class, 0, 10)
	tomap := make(map[string]bool)
	for _, t := range p.txns {
		if t.skipClassification {
			continue
		}
		tomap[t.To] = true
	}
	for to := range tomap {
		p.classes = append(p.classes, bayesian.Class(to))
	}
	assert(len(p.classes) > 0)

	p.cl = bayesian.NewClassifierTfIdf(p.classes...)
	assert(p.cl != nil)
	for _, t := range p.txns {
		if _, has := tomap[t.To]; !has {
			continue
		}
		desc := strings.ToLower(t.Desc)
		p.cl.Learn(strings.Split(desc, " "), bayesian.Class(t.To))
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
		check(err, "Unable to read file: %v", fname)
		final = append(final, include...)
	}
	return final
}

func parseDate(col string) (time.Time, bool) {
	tm, err := time.Parse(*dateFormat, col)
	if err == nil {
		return tm, true
	}
	return time.Time{}, false
}

func parseCurrency(col string) (float64, bool) {
	f, err := strconv.ParseFloat(col, 64)
	return f, err == nil
}

func parseDescription(col string) (string, bool) {
	return strings.Map(func(r rune) rune {
		if r == '"' {
			return -1
		}
		return r
	}, col), true
}

func parseTransactionsFromCSV(in []byte) []txn {
	ignored := make(map[int]bool)
	if len(*ignore) > 0 {
		for _, i := range strings.Split(*ignore, ",") {
			pos, err := strconv.Atoi(i)
			check(err, "Unable to convert to integer: %v", i)
			ignored[pos] = true
		}
	}

	result := make([]txn, 0, 100)
	r := csv.NewReader(bytes.NewReader(in))
	var t txn
	for {
		t = txn{Key: make([]byte, 16)}
		// Have a unique key for each transaction in CSV, so we can unique identify and
		// persist them as we modify their category.
		_, err := rand.Read(t.Key)
		cols, err := r.Read()
		if err == io.EOF {
			break
		}
		check(err, "Unable to read line")

		for i, col := range cols {
			if ignored[i] {
				continue
			}
			if date, ok := parseDate(col); ok {
				t.Date = date

			} else if f, ok := parseCurrency(col); ok {
				t.Cur = f

			} else if d, ok := parseDescription(col); ok {
				t.Desc = d
			}
		}
		if len(t.Desc) != 0 && !t.Date.IsZero() && t.Cur != 0.0 {
			result = append(result, t)
		} else {
			check(fmt.Errorf("Unable to parse txn for [%v]. Got: %+v\n", cols, t), "")
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

func setDefaultMappings(ks *keys.Shortcuts) {
	ks.BestEffortAssign('b', ".back", "default")
	ks.BestEffortAssign('q', ".quit", "default")
	ks.BestEffortAssign('a', ".show all", "default")
	ks.BestEffortAssign('s', ".skip", "default")
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

func singleCharMode() {
	// disable input buffering
	exec.Command("stty", "-F", "/dev/tty", "cbreak", "min", "1").Run()
	// do not display entered characters on the screen
	exec.Command("stty", "-F", "/dev/tty", "-echo").Run()
}

func printCategory(t txn) {
	prefix := "[TO]"
	cat := t.To
	if t.Cur > 0 {
		prefix = "[FROM]"
		cat = t.From
	}
	if len(cat) == 0 {
		return
	}
	if len(cat) > 20 {
		cat = cat[len(cat)-20:]
	}
	color.New(color.BgGreen, color.FgBlack).Printf(" %6s %-20s ", prefix, cat)
}

func printSummary(t txn, idx, total int) {
	if total > 999 {
		color.New(color.BgBlue, color.FgWhite).Printf(" [%4d of %4d] ", idx, total)
	} else if total > 99 {
		color.New(color.BgBlue, color.FgWhite).Printf(" [%3d of %3d] ", idx, total)
	} else if total > 0 {
		color.New(color.BgBlue, color.FgWhite).Printf(" [%2d of %2d] ", idx, total)
	} else if total == 0 {
		// A bit of a hack, but will do.
		color.New(color.BgBlue, color.FgWhite).Printf(" [DUPLICATE] ")
	} else {
		log.Fatalf("Unhandled case for total: %v", total)
	}

	color.New(color.BgYellow, color.FgBlack).Printf(" %10s ", t.Date.Format(stamp))
	desc := t.Desc
	if len(desc) > descLength {
		desc = desc[:descLength]
	}
	color.New(color.BgWhite, color.FgBlack).Printf(" %-40s", desc) // descLength used in Printf.
	printCategory(t)

	color.New(color.BgRed, color.FgWhite).Printf(" %9.2f %3s ", t.Cur, t.CurName)
	fmt.Println()
}

func clear() {
	cmd := exec.Command("clear")
	cmd.Stdout = os.Stdout
	cmd.Run()
	fmt.Println()
}

func (p *parser) writeToDB(t txn) {
	if err := p.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		var val bytes.Buffer
		enc := gob.NewEncoder(&val)
		check(enc.Encode(t), "Unable to encode txn: %v", t)
		return b.Put(t.Key, val.Bytes())

	}); err != nil {
		log.Fatalf("Write to db failed with error: %v", err)
	}
}

func (p *parser) iterateDB() []txn {
	var txns []txn
	if err := p.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t txn
			dec := gob.NewDecoder(bytes.NewBuffer(v))
			if err := dec.Decode(&t); err != nil {
				log.Fatalf("Unable to parse txn from value of length: %v. Error: %v", len(v), err)
			}
			txns = append(txns, t)
		}
		return nil
	}); err != nil {
		log.Fatalf("Iterate over db failed with error: %v", err)
	}
	return txns
}

func (p *parser) printAndGetResult(ks keys.Shortcuts, t *txn) int {
	ks.Print("default", false)

	r := make([]byte, 1)
	os.Stdin.Read(r)
	ch := rune(r[0])
	if ch == rune(10) && len(t.To) > 0 && len(t.From) > 0 {
		p.writeToDB(*t)
		return 1
	}

	if opt, has := ks.MapsTo(ch, "default"); has {
		switch opt {
		case ".back":
			return -1
		case ".skip":
			return 1
		case ".quit":
			return 9999
		case ".show all":
			return math.MaxInt16
		}
		if t.Cur > 0 {
			t.From = opt
		} else {
			t.To = opt
		}
	}
	return 0
}

func (p *parser) printTxn(t *txn, idx, total int) int {
	clear()
	printSummary(*t, idx, total)
	if len(t.Desc) > descLength {
		fmt.Println()
		color.New(color.BgWhite, color.FgBlack).Printf("[DESC] %s ", t.Desc) // descLength used in Printf.
		fmt.Println()
		fmt.Println()
	}

	hits := p.topHits(t.Desc)
	var ks keys.Shortcuts
	setDefaultMappings(&ks)
	for _, hit := range hits {
		ks.AutoAssign(string(hit), "default")
	}
	res := p.printAndGetResult(ks, t)
	if res != math.MaxInt16 {
		return res
	}

	clear()
	printSummary(*t, idx, total)
	res = p.printAndGetResult(*short, t)
	return res
}

func (p *parser) showAndCategorizeTxns(txns []txn) {
	for {
		for i := range txns {
			t := &txns[i]
			hits := p.topHits(t.Desc)
			if t.Cur < 0 {
				t.To = string(hits[0])
			} else {
				t.From = string(hits[0])
			}
			printSummary(*t, i, len(txns))
		}
		fmt.Println()

		fmt.Printf("Found %d transactions. Review (Y/n/q)? ", len(txns))
		b := make([]byte, 1)
		os.Stdin.Read(b)
		if b[0] == 'n' || b[0] == 'q' {
			return
		}

		for i := 0; i < len(txns) && i >= 0; {
			i += p.printTxn(&txns[i], i, len(txns))
		}
	}
}

func ledgerFormat(t txn) string {
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf("%s\t%s\n", t.Date.Format(stamp), t.Desc))
	b.WriteString(fmt.Sprintf("\t%-20s\t%.2f%s\n", t.To, math.Abs(t.Cur), t.CurName))
	b.WriteString(fmt.Sprintf("\t%s\n\n", t.From))
	return b.String()
}

func sanitize(a string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r
		}
		if r >= '0' && r <= '9' {
			return r
		}
		switch r {
		case '*':
			fallthrough
		case ':':
			fallthrough
		case '/':
			fallthrough
		case '.':
			fallthrough
		case '-':
			return r
		default:
			return -1
		}
		return -1
	}, a)
}

func (p *parser) removeDuplicates(txns []txn) []txn {
	if len(txns) == 0 {
		return txns
	}

	sort.Sort(byTime(p.txns))
	sort.Sort(byTime(txns))

	prev := p.txns
	first := txns[0].Date.Add(-24 * time.Hour)
	for i, t := range p.txns {
		if t.Date.After(first) {
			prev = p.txns[i:]
			break
		}
	}

	final := txns[:0]
	for _, t := range txns {
		var found bool
		tdesc := sanitize(t.Desc)
		for _, pr := range prev {
			if pr.Date.After(t.Date) {
				break
			}
			pdesc := sanitize(pr.Desc)
			if tdesc == pdesc && pr.Date.Equal(t.Date) && math.Abs(pr.Cur) == math.Abs(t.Cur) {
				printSummary(t, 0, 0)
				found = true
				break
			}
		}
		if !found {
			final = append(final, t)
		}
	}
	fmt.Printf("\t%d duplicates found and ignored.\n\n", len(txns)-len(final))
	return final
}

var errc = color.New(color.BgRed, color.FgWhite).PrintfFunc()

func oerr(msg string) {
	flag.PrintDefaults()
	fmt.Println()
	errc("\tERROR: " + msg + " ")
	fmt.Println()
}

func main() {
	singleCharMode()
	flag.Parse()

	check(os.MkdirAll(*configDir, 0755), "Unable to create directory: %v", *configDir)
	keyfile := path.Join(*configDir, "shortcuts.yaml")
	short = keys.ParseConfig(keyfile)
	setDefaultMappings(short)
	defer short.Persist(keyfile)

	if len(*account) == 0 {
		oerr("Please specify the account transactions are coming from")
		return
	}

	var config *accountConfig
	configPath := path.Join(*configDir, "config.yaml")
	data, err := ioutil.ReadFile(configPath)
	if err == nil {
		var c configs
		check(yaml.Unmarshal(data, &c), "Unable to unmarshal yaml config at %v", configPath)
		if ac, has := c.Accounts[*account]; has {
			fmt.Printf("Using config: %+v\n", ac)
			config = &ac

			// Allow overwrite of the config if flags are present.
			if len(config.DateFormat) == 0 {
				// We have a default value attached to dateFormat flag, so treat differently.
				config.DateFormat = *dateFormat
			}
			if len(*journal) > 0 {
				config.Journal = *journal
			}
			if len(*currency) > 0 {
				config.Currency = *currency
			}
			if len(*ignore) > 0 {
				config.Ignore = *ignore
			}
			if len(*output) > 0 {
				config.Output = *output
			}
		}
	}

	if config == nil {
		fmt.Printf("No config found in %s. Using all flags", configPath)
		config = &accountConfig{
			Currency:   *currency,
			Journal:    *journal,
			DateFormat: *dateFormat,
			Ignore:     *ignore,
			Output:     *output,
		}
	}

	if len(config.Journal) == 0 {
		oerr("Please specify the input ledger journal file")
		return
	}
	data, err = ioutil.ReadFile(config.Journal)
	check(err, "Unable to read file: %v", config.Journal)
	alldata := includeAll(path.Dir(config.Journal), data)

	if len(config.Output) == 0 {
		oerr("Please specify the output file")
		return
	}
	if _, err := os.Stat(config.Output); os.IsNotExist(err) {
		_, err := os.Create(config.Output)
		check(err, "Unable to check for output file: %v", config.Output)
	}

	tf, err := ioutil.TempFile("", "ledger-csv-txns")
	check(err, "Unable to create temp file")
	defer os.Remove(tf.Name())

	db, err := bolt.Open(tf.Name(), 0600, nil)
	check(err, "Unable to open boltdb at %v", tf.Name())
	defer db.Close()

	db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		check(err, "Unable to create default bucket in boltdb.")
		return nil
	})

	of, err := os.OpenFile(config.Output, os.O_APPEND|os.O_WRONLY, 0600)
	check(err, "Unable to open output file: %v", config.Output)

	p := parser{data: alldata, db: db}
	p.parseAccounts()
	p.parseTransactions(config.Journal)

	// Scanning done. Now train classifier.
	p.generateClasses()

	in, err := ioutil.ReadFile(*csvFile)
	check(err, "Unable to read csv file: %v", *csvFile)
	txns := parseTransactionsFromCSV(in)
	for i := range txns {
		if txns[i].Cur > 0 {
			txns[i].To = *account
		} else {
			txns[i].From = *account
		}
		txns[i].CurName = config.Currency
	}

	txns = p.removeDuplicates(txns)
	p.showAndCategorizeTxns(txns)

	final := p.iterateDB()
	sort.Sort(byTime(final))

	_, err = of.WriteString(fmt.Sprintf("; into-ledger run at %v\n\n", time.Now()))
	check(err, "Unable to write into output file: %v", of.Name())

	for _, t := range final {
		if _, err := of.WriteString(ledgerFormat(t)); err != nil {
			log.Fatalf("Unable to write to output: %v", err)
		}
	}
	check(of.Close(), "Unable to close output file: %v", of.Name())
}

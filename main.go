package main

import (
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
	"strings"
	"text/template"
	"time"

	yaml "gopkg.in/yaml.v2"

	"github.com/boltdb/bolt"
	"github.com/fatih/color"
	"github.com/jbrukh/bayesian"
	"github.com/manishrjain/keys"
)

const defaultTxnTemplateString = "{{.Date.Format \"2006/01/02\"}}\t{{.Payee}}\n\t{{.To | printf \"%-20s\"}}\t{{.Amount}}{{.Currency}}\n\t{{.From}}\n\n"

/// Name for a pseudo-account holding common configuration to all accounts
const commonAccount = "_"

var (
	debug        = flag.Bool("debug", false, "Additional debug information if set.")
	journal      = flag.String("j", "", "Existing journal to learn from.")
	output       = flag.String("o", "out.ldg", "Journal file to write to.")
	csvFile      = flag.String("csv", "", "File path of CSV file containing new transactions.")
	comma        = flag.String("comma", ",", "Separator of fields in csv file")
	ledgerOption = flag.String("opt", "", "Extra option to pass to ledger commands")
	account      = flag.String("a", "", "Name of bank account transactions belong to.")
	currency     = flag.String("c", "", "Set currency if any.")
	ignore       = flag.String("ic", "", "Comma separated list of columns to ignore in CSV.")
	dateFormat   = flag.String("d", "01/02/2006",
		"Express your date format in numeric form w.r.t. Jan 02, 2006, separated by slashes (/). See: https://golang.org/pkg/time/")
	skip      = flag.Int("s", 0, "Number of header lines in CSV to skip")
	configDir = flag.String("conf", os.Getenv("HOME")+"/.into-ledger",
		"Config directory to store various into-ledger configs in.")
	txnTemplateString = flag.String("txnTemplate", defaultTxnTemplateString,
		"Go template to use to produce transactions in the ledger journal")
	shortcuts = flag.String("short", "shortcuts.yaml", "Name of shortcuts file.")

	pstart = time.Now().Add(-90 * 24 * time.Hour).Format(plaidDate)
	pend   = time.Now().Format(plaidDate)

	// The following flags are for using Plaid.com integration to auto-fetch txns.
	usePlaid = flag.Bool("p", false, "Use Plaid to auto-fetch txns."+
		" You must have set plaid.yaml in conf dir.")

	dupWithin = flag.Int("within", 24, "Consider txns to be dups, if their dates are not"+
		" more than N hours apart. Description and amount must also match exactly for"+
		" a txn to be considered duplicate.")

	smallBelow = flag.Float64("below", 0.0, "Use Expenses:Small category for txns below this amount.")

	rtxn   = regexp.MustCompile(`(\d{4}/\d{2}/\d{2})[\W]*(\w.*)`)
	rto    = regexp.MustCompile(`\W*([:\w]+)(.*)`)
	rfrom  = regexp.MustCompile(`\W*([:\w]+).*`)
	rcur   = regexp.MustCompile(`(\d+\.\d+|\d+)`)
	racc   = regexp.MustCompile(`^account[\W]+(.*)`)
	ralias = regexp.MustCompile(`\balias\s(.*)`)

	stamp       = "2006/01/02"
	bucketName  = []byte("txns")
	descLength  = 40
	catLength   = 20
	short       *keys.Shortcuts
	txnTemplate *template.Template
)

type configs struct {
	Accounts map[string]map[string]string // account and the corresponding config.
}

type Txn struct {
	/// Date of the transaction
	Date time.Time

	/// Payee, extracted from the description of the CSV file most of the time
	Desc string

	/// Account to money is going into, like 'Expenses:…'
	To string

	/// Account the money is coming from, like 'Assets:…'
	From string

	/// Amount
	Cur float64

	/// Currency, like 'USD'
	CurName string

	/// For internal use
	Key []byte

	skipClassification bool
	Done               bool
}

type byTime []Txn

func (b byTime) Len() int               { return len(b) }
func (b byTime) Less(i int, j int) bool { return b[i].Date.Before(b[j].Date) }
func (b byTime) Swap(i int, j int)      { b[i], b[j] = b[j], b[i] }

func assignForAccount(account string) {
	tree := strings.Split(account, ":")
	assertf(len(tree) > 0, "Expected at least one result. Found none for: %v", account)
	short.AutoAssign(tree[0], "default")
	prev := tree[0]
	for _, c := range tree[1:] {
		if len(c) == 0 {
			continue
		}
		short.AutoAssign(c, prev)
		prev = c
	}
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

func saneMode() {
	exec.Command("stty", "-F", "/dev/tty", "sane").Run()
}

func getCategory(t Txn) (prefix, cat string) {
	prefix = "[TO]"
	cat = t.To
	if t.Cur > 0 {
		prefix = "[FROM]"
		cat = t.From
	}
	return
}

func printCategory(t Txn) {
	prefix, cat := getCategory(t)
	if len(cat) == 0 {
		return
	}
	if len(cat) > catLength {
		cat = cat[len(cat)-catLength:]
	}
	color.New(color.BgGreen, color.FgBlack).Printf(" %6s %-20s ", prefix, cat)
}

func printSummary(t Txn, idx, total int) {
	if t.Done {
		color.New(color.BgGreen, color.FgBlack).Printf(" R ")
	} else {
		color.New(color.BgRed, color.FgWhite).Printf(" N ")
	}

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

/// Transaction structure for templating
type TxnTemplate struct {
	Date     time.Time
	Payee    string
	To       string
	From     string
	Amount   float64
	Currency string
}

func toTxnTemplate(t Txn) TxnTemplate {
	var tt TxnTemplate
	tt.Date = t.Date
	tt.Payee = t.Desc
	tt.To = t.To
	tt.From = t.From
	tt.Amount = t.Cur
	tt.Currency = t.CurName
	return tt
}

/// ledgerFormat formats a string for insertion into a ledger journal, using
/// provided template.
func ledgerFormat(t Txn, tmpl *template.Template) string {
	if *debug {
		fmt.Printf("ledgerFormat: tmpl: %v\n", *tmpl)
	}
	var b strings.Builder
	// var b bytes.Buffer
	tmpl.Execute(&b, toTxnTemplate(t))
	return b.String()
}

func main() {
	flag.Parse()

	if *plaidHist != "" {
		fmt.Printf("Balance history error: %v\n", BalanceHistory(*account))
		return
	}

	defer saneMode()
	singleCharMode()

	checkf(os.MkdirAll(*configDir, 0755), "Unable to create directory: %v", *configDir)
	if len(*account) == 0 {
		oerr("Please specify the account transactions are coming from")
		return
	}

	configPath := path.Join(*configDir, "config.yaml")
	data, err := ioutil.ReadFile(configPath)
	if err == nil {
		var c configs
		checkf(yaml.Unmarshal(data, &c), "Unable to unmarshal yaml config at %v", configPath)
		if ac, has := c.Accounts[*account]; has {
			fmt.Printf("Using flags from config: %+v\n", ac)
			/// Merge common setting before using settings for this account
			if defaultAc, has := c.Accounts[commonAccount]; has {
				if *debug {
					fmt.Println("Setting common flags")
				}
				for k, v := range defaultAc {
					flag.Set(k, v)
					if *debug {
						fmt.Printf("Set common flag '%v' to '%v'\n", k, v)
					}
				}
			}
			if *debug {
				fmt.Printf("Setting flags for account %v\n", *account)
			}
			for k, v := range ac {
				flag.Set(k, v)
			}
		} else if *debug {
			fmt.Println("No flag set from config")
		}
	} else if *debug {
		fmt.Printf("No config file found at %v\n", configPath)
	}
	keyfile := path.Join(*configDir, *shortcuts)
	short = keys.ParseConfig(keyfile)
	setDefaultMappings(short)
	defer short.Persist(keyfile)

	if len(*journal) == 0 {
		oerr("Please specify the input ledger journal file")
		return
	}
	data, err = ioutil.ReadFile(*journal)
	checkf(err, "Unable to read file: %v", *journal)
	alldata := includeAll(path.Dir(*journal), data)

	txnTemplate, err = template.New("transaction").Parse(*txnTemplateString)
	checkf(err, "Unable to parse transaction template %v", *txnTemplateString)

	if len(*output) == 0 {
		oerr("Please specify the output file")
		return
	}
	if _, err := os.Stat(*output); os.IsNotExist(err) {
		_, err := os.Create(*output)
		checkf(err, "Unable to check for output file: %v", *output)
	}

	tf, err := ioutil.TempFile("", "ledger-csv-txns")
	checkf(err, "Unable to create temp file")
	defer os.Remove(tf.Name())

	db, err := bolt.Open(tf.Name(), 0600, nil)
	checkf(err, "Unable to open boltdb at %v", tf.Name())
	defer db.Close()

	db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		checkf(err, "Unable to create default bucket in boltdb.")
		return nil
	})

	of, err := os.OpenFile(*output, os.O_APPEND|os.O_WRONLY, 0600)
	checkf(err, "Unable to open output file: %v", *output)

	p := parser{data: alldata, db: db}
	p.parseAccounts()
	p.parseTransactions()

	// Scanning done. Now train classifier.
	p.generateClasses()

	var txns []Txn
	switch {
	case *usePlaid:
		var err error
		txns, err = GetPlaidTransactions(*account)
		checkf(err, "Couldn't get plaid txns")

	case len(*csvFile) > 0:
		in, err := ioutil.ReadFile(*csvFile)
		checkf(err, "Unable to read csv file: %v", *csvFile)
		txns = parseTransactionsFromCSV(in)

	default:
		assertf(false, "Please specify either a CSV flag or a Plaid flag")
	}

	for i := range txns {
		txns[i].CurName = *currency
		if txns[i].Cur > 0 {
			txns[i].To = *account
		} else {
			txns[i].From = *account
		}
	}
	if len(txns) > 0 {
		sort.Sort(byTime(txns))
		fmt.Println("Earliest and Latest transactions:")
		printSummary(txns[0], 1, 2)
		printSummary(txns[len(txns)-1], 2, 2)
		fmt.Println()
	}

	txns = p.removeDuplicates(txns) // sorts by date.

	// Now sort by description for the rest of the categorizers.
	sort.Slice(txns, func(i, j int) bool {
		di := lettersOnly.ReplaceAllString(txns[i].Desc, "")
		dj := lettersOnly.ReplaceAllString(txns[j].Desc, "")
		cmp := strings.Compare(di, dj)
		if cmp != 0 {
			return cmp < 0
		}
		return txns[i].Date.After(txns[j].Date)
	})
	txns = p.categorizeByRules(txns)
	txns = p.categorizeBelow(txns)
	p.showAndCategorizeTxns(txns)

	final := p.iterateDB()
	sort.Sort(byTime(final))

	_, err = of.WriteString(fmt.Sprintf("; into-ledger run at %v\n\n", time.Now()))
	checkf(err, "Unable to write into output file: %v", of.Name())

	for _, t := range final {
		if _, err := of.WriteString(ledgerFormat(t, txnTemplate)); err != nil {
			log.Fatalf("Unable to write to output: %v", err)
		}
	}
	fmt.Printf("Transactions written to file: %s\n", of.Name())
	checkf(of.Close(), "Unable to close output file: %v", of.Name())
}

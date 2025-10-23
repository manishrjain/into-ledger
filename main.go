package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/boltdb/bolt"
	"github.com/fatih/color"
	"github.com/jbrukh/bayesian"
	"github.com/manishrjain/keys"
	"github.com/pkg/errors"
)

var (
	debug      = flag.Bool("debug", true, "Additional debug information if set.")
	journal    = flag.String("in", "", "Existing journal to learn from.")
	output     = flag.String("out", "", "Journal file to write to. Defaults to the journal file (-j) if not specified.")
	csvFile    = flag.String("csv", "", "File path of CSV file containing new transactions.")
	account    = flag.String("account", "", "Account name (e.g., 'Assets:Checking') or CSV column index (e.g., '6') for account field. When using column index, add csv-account mappings in ledger file.")
	currency   = flag.String("currency", "$", "Set currency if any.")
	ignore     = flag.String("cols-ignore", "", "Comma separated list of columns to ignore in CSV.")
	selectCols = flag.String("cols-select", "", "Comma separated list of columns to select from CSV (e.g., '0,1,5' for columns 0, 1, and 5).")
	dateFormat = flag.String("date", "01/02/2006",
		"Express your date format in numeric form w.r.t. Jan 02, 2006, separated by slashes (/). See: https://golang.org/pkg/time/")
	skip      = flag.Int("skip", 0, "Number of header lines in CSV to skip")
	configDir = flag.String("conf", os.Getenv("HOME")+"/.into-ledger",
		"Config directory to store various into-ledger configs in.")
	shortcuts         = flag.String("short", "shortcuts.yaml", "Name of shortcuts file.")
	smallBelow        = flag.Float64("below", 0.0, "Use Expenses:Small category for txns below this amount.")
	aiReview          = flag.Bool("ai-review", true, "Use Claude AI to automatically review and categorize transactions")
	bayesianThreshold = flag.Float64("bayesian-threshold", 1.1, "Auto-approve Bayesian predictions above this confidence (0.0-1.0). Set higher to send more transactions to AI review.")
	batchSize         = flag.Int("batch-size", 50, "Number of transactions to send to Claude API per batch. Max 8192 tokens ÷ 150 tokens/decision ≈ 54, using 50 for safety.")
	limitTxns         = flag.Int("limit", 50, "Maximum number of transactions to process per run after deduplication (0 = unlimited). Helps avoid losing work on crashes.")
	dupWithin         = flag.Int("within", 24, "Consider txns to be dups, if their dates are not"+
		" more than N hours apart. Description and amount must also match exactly for"+
		" a txn to be considered duplicate.")

	racc = regexp.MustCompile(`^account[\W]+(.*)`)

	debugPrefix = "_debug.batch"
	stamp       = "2006/01/02"
	bucketName  = []byte("txns")
	descLength  = 40
	catLength   = 20
	short       *keys.Shortcuts
)

type configs struct {
	Accounts map[string]map[string]string // account and the corresponding config.
	AI       struct {
		Enabled bool   `yaml:"enabled"`
		APIKey  string `yaml:"api_key"`
		Model   string `yaml:"model"`
	} `yaml:"ai"`
}

// CategoryScore represents a category with its confidence score
type CategoryScore struct {
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
}

// ReviewTransaction represents a transaction for AI review
type ReviewTransaction struct {
	Date        string          `json:"date"`
	Description string          `json:"description"`
	Amount      float64         `json:"amount"`
	Currency    string          `json:"currency"`
	Account     string          `json:"account"`
	Categories  []CategoryScore `json:"categories"`
}

// ExampleTransaction represents a sample transaction for a category
type ExampleTransaction struct {
	Date        string  `json:"date"`
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
}

// CategoryInfo represents a category with its metadata
type CategoryInfo struct {
	Name     string               `json:"name"`
	Comment  string               `json:"comment,omitempty"`
	Examples []ExampleTransaction `json:"examples,omitempty"`
}

// ReviewData is the structure sent to AI for review
type ReviewData struct {
	Transactions  []ReviewTransaction `json:"transactions"`
	AllCategories []CategoryInfo      `json:"all_categories"`
}

// AIDecision represents the AI's categorization decision for a transaction
type AIDecision struct {
	SuggestedCategories []CategoryScore `json:"suggested_categories"` // Up to 3 categories with confidence scores, sorted by confidence
	Source              string          `json:"source"`               // "ai" or "uncertain"
	Reasoning           string          `json:"reasoning,omitempty"`
}

// AIResponse is the response from Claude API
type AIResponse struct {
	Decisions []AIDecision `json:"decisions"`
}

type Txn struct {
	Date               time.Time
	Desc               string
	To                 string
	From               string
	Cur                float64
	CurName            string
	Key                []byte
	skipClassification bool
	Done               bool
	Account            string          // Account from CSV (e.g., "Chase Bank - JAIN CHK (8987)")
	AIReason           string          // AI's reasoning for categorization (for manual review context)
	AISuggestions      []CategoryScore // AI suggested categories with confidence scores (up to 3)
}

type byTime []Txn

func (b byTime) Len() int               { return len(b) }
func (b byTime) Less(i int, j int) bool { return b[i].Date.Before(b[j].Date) }
func (b byTime) Swap(i int, j int)      { b[i], b[j] = b[j], b[i] }

func checkf(err error, format string, args ...any) {
	if err != nil {
		log.Printf(format, args...)
		log.Println()
		log.Fatalf("%+v", errors.WithStack(err))
	}
}

func assertf(ok bool, format string, args ...any) {
	if !ok {
		log.Printf(format, args...)
		log.Println()
		log.Fatalf("%+v", errors.Errorf("Should be true, but is false"))
	}
}

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

type parser struct {
	db              *bolt.DB
	data            []byte
	txns            []Txn
	classes         []bayesian.Class
	cl              *bayesian.Classifier
	accounts        []string
	accountMapping  map[string]string // maps CSV account identifiers to ledger accounts
	accountComments map[string]string // maps account names to their comments/descriptions
}

func (p *parser) parseTransactions() {
	out, err := exec.Command("ledger", "-f", *journal, "csv").Output()
	checkf(err, "Unable to convert journal to csv. Possibly an issue with your ledger installation.")
	r := csv.NewReader(newConverter(bytes.NewReader(out)))
	var t Txn
	for {
		cols, err := r.Read()
		if err == io.EOF {
			break
		}
		checkf(err, "Unable to read a csv line.")

		t = Txn{}
		t.Date, err = time.Parse(stamp, cols[0])
		checkf(err, "Unable to parse time: %v", cols[0])
		t.Desc = strings.Trim(cols[2], " \n\t")

		t.To = cols[3]
		assertf(len(t.To) > 0, "Expected TO, found empty.")
		if strings.HasPrefix(t.To, "Assets:Reimbursements:") {
			// pass
		} else if strings.HasPrefix(t.To, "Assets:") {
			// Don't pick up Assets.
			t.skipClassification = true
		} else if strings.HasPrefix(t.To, "Equity:") {
			// Don't pick up Equity.
			t.skipClassification = true
		} else if strings.HasPrefix(t.To, "Liabilities:") {
			// Don't pick up Liabilities.
			t.skipClassification = true
		}
		t.CurName = cols[4]
		t.Cur, err = strconv.ParseFloat(cols[5], 64)
		checkf(err, "Unable to parse amount.")
		p.txns = append(p.txns, t)

		assignForAccount(t.To)
	}
}

func (p *parser) parseAccounts() {
	p.accountComments = make(map[string]string)
	s := bufio.NewScanner(bytes.NewReader(p.data))
	var acc string
	var comments []string
	rcomment := regexp.MustCompile(`^\s*;(.*)`)

	for s.Scan() {
		line := s.Text()

		// Check if this is an account declaration
		m := racc.FindStringSubmatch(line)
		if len(m) >= 2 && len(m[1]) > 0 {
			// Save previous account with its comments if any
			if len(acc) > 0 && len(comments) > 0 {
				p.accountComments[acc] = strings.Join(comments, " ")
			}

			// Start new account
			acc = m[1]
			comments = nil
			p.accounts = append(p.accounts, acc)
			assignForAccount(acc)
			continue
		}

		// Check if this is a comment line (for the current account)
		if len(acc) > 0 {
			m := rcomment.FindStringSubmatch(line)
			if len(m) >= 2 {
				comment := strings.TrimSpace(m[1])
				// Skip csv-account mapping comments (handled separately)
				if !strings.HasPrefix(comment, "csv-account:") && len(comment) > 0 {
					comments = append(comments, comment)
				}
			} else if len(strings.TrimSpace(line)) == 0 && len(comments) > 0 {
				// Empty line marks end of account declaration
				p.accountComments[acc] = strings.Join(comments, " ")
				comments = nil
				acc = ""
			}
		}
	}

	// Save last account's comments if any
	if len(acc) > 0 && len(comments) > 0 {
		p.accountComments[acc] = strings.Join(comments, " ")
	}

	if *debug && len(p.accountComments) > 0 {
		fmt.Printf("[Account Comments] Found %d accounts with descriptions\n", len(p.accountComments))
	}
}

// parseAccountMappings parses comments in the ledger file to find mappings
// from CSV account identifiers to ledger accounts.
// Expected format in ledger file:
//
//	account Assets:Checking
//	  ; csv-account: CHK
//	  ; csv-account: checking
func (p *parser) parseAccountMappings() {
	p.accountMapping = make(map[string]string)
	s := bufio.NewScanner(bytes.NewReader(p.data))
	var currentAccount string
	rcsvAccount := regexp.MustCompile(`;\s*csv-account:\s*(.+)`)

	for s.Scan() {
		line := s.Text()

		// Check if this is an account declaration
		m := racc.FindStringSubmatch(line)
		if len(m) >= 2 && len(m[1]) > 0 {
			currentAccount = m[1]
			continue
		}

		// Check if this is a csv-account mapping comment
		if currentAccount != "" {
			m := rcsvAccount.FindStringSubmatch(line)
			if len(m) >= 2 {
				identifier := strings.TrimSpace(m[1])
				if len(identifier) > 0 {
					// Store mapping (case-insensitive)
					p.accountMapping[strings.ToLower(identifier)] = currentAccount
					if *debug {
						fmt.Printf("[Account Mapping] '%s' -> %s\n", identifier, currentAccount)
					}
				}
			}
		}
	}
}

// matchAccountToLedger matches a CSV account string to a ledger account
// using the account mappings parsed from the ledger file.
func (p *parser) matchAccountToLedger(csvAccount string) string {
	if csvAccount == "" {
		return ""
	}

	csvLower := strings.ToLower(csvAccount)

	// Try exact match first
	if ledgerAccount, ok := p.accountMapping[csvLower]; ok {
		return ledgerAccount
	}

	// Try substring match - check if any mapping key is contained in the CSV account
	for key, ledgerAccount := range p.accountMapping {
		if strings.Contains(csvLower, key) {
			return ledgerAccount
		}
	}

	return ""
}

// prepareDescriptionForClassification prepares a description for Bayesian classification
// by converting to lowercase, removing noise words, and splitting into terms
func prepareDescriptionForClassification(desc string) []string {
	desc = strings.ToLower(desc)

	// Remove "privacycom" keyword (case-insensitive)
	// This handles cases like "Privacycom *Merchant" or "*Privacycom Merchant"
	desc = strings.ReplaceAll(desc, "privacycom", " ")
	desc = strings.ReplaceAll(desc, "*", " ")

	// Split and filter empty strings
	terms := strings.Fields(desc) // Fields splits on whitespace and removes empty strings
	return terms
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
	for class := range tomap {
		fmt.Printf("[Class] %s\n", class)
	}
	for to := range tomap {
		p.classes = append(p.classes, bayesian.Class(to))
	}
	assertf(len(p.classes) > 1, "Expected some categories. Found none.")

	p.cl = bayesian.NewClassifierTfIdf(p.classes...)
	assertf(p.cl != nil, "Expected a valid classifier. Found nil.")
	for _, t := range p.txns {
		if _, has := tomap[t.To]; !has {
			continue
		}
		terms := prepareDescriptionForClassification(t.Desc)
		p.cl.Learn(terms, bayesian.Class(t.To))
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
	terms := prepareDescriptionForClassification(in)
	scores, _, _ := p.cl.LogScores(terms)
	pairs := make([]pair, 0, len(scores))

	var mean, stddev float64
	for pos, score := range scores {
		pairs = append(pairs, pair{score, pos})
		mean += score
	}
	mean /= float64(len(scores))
	for _, score := range scores {
		diff := score - mean
		stddev += diff * diff
	}
	stddev /= float64(len(scores) - 1)
	stddev = math.Sqrt(stddev)

	sort.Sort(byScore(pairs))
	result := make([]bayesian.Class, 0, 5)
	last := pairs[0].score
	maxResults := min(len(pairs), 5)
	for i := range maxResults {
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
		include, err := os.ReadFile(path.Join(dir, fname))
		checkf(err, "Unable to read file: %v", fname)
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

func parseTransactionsFromCSV(in []byte, accountColIdx int) []Txn {
	// Read first line to determine total number of columns
	r := csv.NewReader(bytes.NewReader(in))
	firstLine, err := r.Read()
	checkf(err, "Unable to read first line of CSV")
	totalCols := len(firstLine)

	// Reset reader
	r = csv.NewReader(bytes.NewReader(in))

	// Build column filter: true = include column, false = exclude column
	columnFilter := make(map[int]bool)

	if len(*selectCols) > 0 {
		// Select mode: reject everything, then select specified columns
		for i := range totalCols {
			columnFilter[i] = false
		}
		for i := range strings.SplitSeq(*selectCols, ",") {
			pos, err := strconv.Atoi(strings.TrimSpace(i))
			checkf(err, "Unable to convert to integer: %v", i)
			if pos < totalCols {
				columnFilter[pos] = true
			}
		}
	} else if len(*ignore) > 0 {
		// Ignore mode: select everything, then reject specified columns
		for i := range totalCols {
			columnFilter[i] = true
		}
		for i := range strings.SplitSeq(*ignore, ",") {
			pos, err := strconv.Atoi(strings.TrimSpace(i))
			checkf(err, "Unable to convert to integer: %v", i)
			if pos < totalCols {
				columnFilter[pos] = false
			}
		}
	} else {
		// No filtering: select all columns
		for i := range totalCols {
			columnFilter[i] = true
		}
	}

	result := make([]Txn, 0, 100)
	var t Txn
	var skipped int
	for {
		t = Txn{Key: make([]byte, 16)}
		t.CurName = *currency // Use default, until we have CSV currency name parsing.

		// Have a unique key for each transaction in CSV, so we can unique identify and
		// persist them as we modify their category.
		if _, err := rand.Read(t.Key); err != nil {
			log.Fatalf("Unable to generate random key: %v", err)
		}
		cols, err := r.Read()
		if err == io.EOF {
			break
		}
		checkf(err, "Unable to read line: %v", strings.Join(cols, ", "))
		if *skip > skipped {
			skipped++
			continue
		}

		var picked []string
		for i, col := range cols {
			// Capture account column if specified
			if accountColIdx >= 0 && i == accountColIdx {
				t.Account = strings.TrimSpace(col)
			}

			// Skip column if it's not selected in the filter
			if !columnFilter[i] {
				continue
			}

			picked = append(picked, col)
			if date, ok := parseDate(col); ok {
				t.Date = date
			} else if f, ok := parseCurrency(col); ok {
				t.Cur = f
			} else if d, ok := parseDescription(col); ok {
				t.Desc = d
			}
		}

		if len(t.Desc) != 0 && !t.Date.IsZero() && t.Cur != 0.0 {
			y, m, d := t.Date.Year(), t.Date.Month(), t.Date.Day()
			t.Date = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
			result = append(result, t)
		} else {
			fmt.Println()
			fmt.Printf("ERROR           : Unable to parse transaction from the selected columns in CSV.\n")
			fmt.Printf("Selected CSV    : %v\n", strings.Join(picked, ", "))
			fmt.Printf("Parsed Date     : %v\n", t.Date)
			fmt.Printf("Parsed Desc     : %v\n", t.Desc)
			fmt.Printf("Parsed Currency : %v\n", t.Cur)
			log.Fatalln("Please ensure that the above CSV contains ALL the 3 required fields.")
		}
	}
	return result
}

func setDefaultMappings(ks *keys.Shortcuts) {
	ks.BestEffortAssign('b', ".back", "default")
	ks.BestEffortAssign('q', ".quit", "default")
	ks.BestEffortAssign('a', ".show all", "default")
	ks.BestEffortAssign('s', ".skip", "default")
	ks.BestEffortAssign('g', ".google", "default")
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

func (p *parser) writeToDB(t Txn) {
	if err := p.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		var val bytes.Buffer
		enc := gob.NewEncoder(&val)
		checkf(enc.Encode(t), "Unable to encode txn: %v", t)
		return b.Put(t.Key, val.Bytes())
	}); err != nil {
		log.Fatalf("Write to db failed with error: %v", err)
	}
}

func (p *parser) iterateDB() []Txn {
	var txns []Txn
	if err := p.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t Txn
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

func (p *parser) printAndGetResult(ks keys.Shortcuts, t *Txn) float64 {
	label := "default"

	var repeat bool
	var category []string
LOOP:
	// At depth 2 (after selecting 2 levels), add TODO as an option for the 3rd level
	if len(category) == 2 {
		ks.AutoAssign("TODO", label)
	}

	if len(category) > 0 {
		fmt.Println()
		color.New(color.BgWhite, color.FgBlack).Printf("Selected [%s]", strings.Join(category, ":")) // descLength used in Printf.
		fmt.Println()
	}

	ks.Print(label, false)
	r := make([]byte, 1)
	os.Stdin.Read(r)
	ch := rune(r[0])
	if ch == rune(10) && len(t.To) > 0 && len(t.From) > 0 {
		p.writeToDB(*t)
		t.Done = true
		if repeat {
			return 0.0
		}
		return 1.0
	}

	if opt, has := ks.MapsTo(ch, label); has {
		switch opt {
		case ".back":
			return -1.0
		case ".skip":
			return 1.1
		case ".quit":
			return 999999.0
		case ".show all":
			return math.MaxFloat32
		case ".google":
			// Open browser with Google search for the transaction description
			searchQuery := strings.ReplaceAll(t.Desc, " ", "+")
			url := fmt.Sprintf("https://www.google.com/search?q=%s", searchQuery)

			// Try to open browser (works on Linux and macOS)
			cmd := exec.Command("xdg-open", url)
			if err := cmd.Start(); err != nil {
				// Try macOS open command
				cmd = exec.Command("open", url)
				if err := cmd.Start(); err != nil {
					fmt.Printf("Could not open browser. Search URL: %s\n", url)
				}
			}
			fmt.Println("Opening Google search in browser...")
			time.Sleep(500 * time.Millisecond) // Brief pause to show message
			return 0                           // Stay on same transaction
		}

		category = append(category, opt)
		if t.Cur > 0 {
			t.From = strings.Join(category, ":")
		} else {
			t.To = strings.Join(category, ":")
		}
		label = opt
		if ks.HasLabel(label) {
			repeat = true
			goto LOOP
		}
	}
	return 0
}

func (p *parser) categorizeTxn(t *Txn, idx, total int) float64 {
	clear()
	printSummary(*t, idx, total)
	fmt.Println()
	if len(t.Desc) > descLength {
		color.New(color.BgWhite, color.FgBlack).Printf("%6s %s ", "[DESC]", t.Desc) // descLength used in Printf.
		fmt.Println()
	}
	{
		prefix, cat := getCategory(*t)
		if len(cat) > catLength {
			color.New(color.BgGreen, color.FgBlack).Printf("%6s %s", prefix, cat)
			fmt.Println()
		}
	}
	// Display AI reasoning if present
	if len(t.AIReason) > 0 {
		color.New(color.BgCyan, color.FgBlack).Printf("[AI] %s", t.AIReason)
		fmt.Println()
	}
	// Display AI suggestions if present
	if len(t.AISuggestions) > 0 {
		color.New(color.BgMagenta, color.FgWhite).Printf("[AI SUGGESTIONS]")
		fmt.Println()
		for i, suggestion := range t.AISuggestions {
			color.New(color.FgCyan).Printf("  %d. ", i+1)
			color.New(color.FgYellow).Printf("%-40s", suggestion.Category)
			color.New(color.FgGreen).Printf(" (%.2f%%)", suggestion.Confidence*100)
			fmt.Println()
		}
	}
	fmt.Println()

	hits := p.topHits(t.Desc)
	var ks keys.Shortcuts
	setDefaultMappings(&ks)

	// Track categories we've already assigned to avoid duplicates
	assigned := make(map[string]bool)

	// First, add AI suggestions if available
	if len(t.AISuggestions) > 0 {
		for _, suggestion := range t.AISuggestions {
			ks.AutoAssign(suggestion.Category, "default")
			assigned[suggestion.Category] = true
		}
	}
	// Then add Bayesian hits (skip if already assigned from AI)
	for _, hit := range hits {
		category := string(hit)
		if !assigned[category] {
			ks.AutoAssign(category, "default")
			assigned[category] = true
		}
	}
	res := p.printAndGetResult(ks, t)
	if res != math.MaxFloat32 {
		return res
	}

	clear()
	printSummary(*t, idx, total)
	return p.printAndGetResult(*short, t)
}

func (p *parser) classifyTxn(t *Txn) {
	// Don't overwrite categories that were already set by AI review
	if len(t.AIReason) > 0 {
		return
	}

	if !t.Done {
		hits := p.topHits(t.Desc)
		if t.Cur < 0 {
			t.To = string(hits[0])
		} else {
			t.From = string(hits[0])
		}
	}
}

var lettersOnly = regexp.MustCompile("[^a-zA-Z]+")

func (p *parser) showAndCategorizeTxns(rtxns []Txn) {
	txns := rtxns
	for {
		for i := range len(txns) {
			t := &txns[i]
			p.classifyTxn(t)
			printSummary(*t, i, len(txns))
		}
		fmt.Println()

		fmt.Printf("Found %d transactions. Review (Y/n/q)? ", len(txns))
		b := make([]byte, 1)
		os.Stdin.Read(b)
		if b[0] == 'n' || b[0] == 'q' {
			return
		}

		applyToSimilarTxns := func(from int) int {
			t := txns[from]
			src := lettersOnly.ReplaceAllString(t.Desc, "")
			for i := from + 1; i < len(txns); i++ {
				dst := &txns[i]
				if src != lettersOnly.ReplaceAllString(dst.Desc, "") {
					return i
				}
				if math.Signbit(t.Cur) != math.Signbit(dst.Cur) {
					return i
				}

				if t.Cur > 0 {
					dst.From = t.From
				} else {
					dst.To = t.To
				}
				dst.Done = true
			}
			return len(txns)
		}

		for i := 0; i < len(txns) && i >= 0; {
			t := &txns[i]
			res := p.categorizeTxn(t, i, len(txns))
			if res == 1.0 {
				upto := applyToSimilarTxns(i)
				if upto == i+1 {
					// Did not find anything.
					i += int(res)
					continue
				}
				clear()
				printSummary(txns[i], i, len(txns))
				for j := i + 1; j < upto; j++ {
					printSummary(txns[j], j, len(txns))
					p.writeToDB(txns[j])
				}
				fmt.Println()
				fmt.Println("The above txns were similar to the last categorized txns, " +
					"and were categorized accordingly. Can be changed by skipping back and forth.")
				r := make([]byte, 1)
				os.Stdin.Read(r)
				i = upto
			} else {
				i += int(res)
			}
		}
	}
}

func ledgerFormat(t Txn) string {
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf("%s\t%s\n", t.Date.Format(stamp), t.Desc))
	// Include AI reasoning as a comment if present
	if len(t.AIReason) > 0 {
		b.WriteString(fmt.Sprintf("    ; %s\n", t.AIReason))
	}
	b.WriteString(fmt.Sprintf("\t%-20s\t%s %.2f\n", t.To, t.CurName, math.Abs(t.Cur)))
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
	}, a)
}

func (p *parser) categorizeBelow(txns []Txn) []Txn {
	unmatched := txns[:0]
	var count int
	var total float64
	for i := range txns {
		txn := &txns[i]
		if txn.Cur < 0 && txn.Cur >= -(*smallBelow) {
			total += txn.Cur
			count++
			txn.To = "Expenses:Small"
			printSummary(*txn, count, count)
			p.writeToDB(*txn)
		} else {
			unmatched = append(unmatched, *txn)
		}
	}
	fmt.Printf("\t%d txns totaling %.2f below %.2f have been categorized as 'Expenses:Small'.\n\n",
		count, math.Abs(total), *smallBelow)
	return unmatched
}

// This function would use a rules.yaml file in this format:
// Expenses:Travel:
//   - regexp-for-description
//   - ^LYFT\ +\*RIDE
//
// Expenses:Food:
//   - ^STARBUCKS
//
// ...
// If this file is present, txns would be auto-categorized, if their description
// mathces the regular expressions provided.
func (p *parser) categorizeByRules(txns []Txn) []Txn {
	fpath := path.Join(*configDir, "rules.yaml")
	data, err := os.ReadFile(fpath)
	if err != nil {
		return txns
	}

	rules := make(map[string][]string)
	checkf(yaml.Unmarshal(data, &rules), "Unable to parse auto.yaml confit at %s", fpath)

	matchesCategory := func(t Txn) string {
		for category, patterns := range rules {
			for _, pattern := range patterns {
				match, err := regexp.Match(pattern, []byte(t.Desc))
				checkf(err, "Unable to parse regexp")
				if match {
					return category
				}
			}
		}
		return ""
	}

	unmatched := txns[:0]
	var count int
	for _, t := range txns {
		if cat := matchesCategory(t); len(cat) > 0 {
			if t.Cur > 0 {
				t.From = cat
			} else {
				t.To = cat
			}
			count++
			printSummary(t, count, count)
			p.writeToDB(t)
		} else {
			unmatched = append(unmatched, t)
		}
	}
	fmt.Printf("\t%d txns have been categorized based on rules.\n\n", len(txns)-len(unmatched))
	return unmatched
}

// selectDiverseExamples selects up to 3 diverse example transactions for a category
// It prioritizes high-confidence transactions with diverse descriptions
func (p *parser) selectDiverseExamples(category string, maxExamples int) []ExampleTransaction {
	var examples []ExampleTransaction

	// Find all transactions in this category
	type txnScore struct {
		txn        Txn
		confidence float64
	}
	var candidates []txnScore

	for _, t := range p.txns {
		if t.To == category {
			// Calculate confidence for this transaction
			terms := prepareDescriptionForClassification(t.Desc)
			scores, _, _ := p.cl.LogScores(terms)

			// Find the score for this category
			for i, class := range p.classes {
				if string(class) == category && i < len(scores) {
					// Normalize using softmax-like approach
					maxScore := scores[0]
					for _, s := range scores {
						if s > maxScore {
							maxScore = s
						}
					}
					confidence := math.Exp(scores[i] - maxScore)
					candidates = append(candidates, txnScore{t, confidence})
					break
				}
			}
		}
	}

	if len(candidates) == 0 {
		return examples
	}

	// Sort by confidence (highest first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].confidence > candidates[j].confidence
	})

	// Select diverse examples by comparing descriptions
	// Use letter-only comparison to check for similarity
	selected := make([]txnScore, 0, maxExamples)
	for _, candidate := range candidates {
		if len(selected) >= maxExamples {
			break
		}

		// Check if this description is sufficiently different from already selected ones
		isDiverse := true
		candidateDesc := lettersOnly.ReplaceAllString(candidate.txn.Desc, "")

		for _, sel := range selected {
			selDesc := lettersOnly.ReplaceAllString(sel.txn.Desc, "")
			if candidateDesc == selDesc {
				isDiverse = false
				break
			}
		}

		if isDiverse {
			selected = append(selected, candidate)
		}
	}

	// Convert to ExampleTransaction format
	for _, s := range selected {
		examples = append(examples, ExampleTransaction{
			Date:        s.txn.Date.Format("2006-01-02"),
			Description: s.txn.Desc,
			Amount:      s.txn.Cur,
		})
	}

	return examples
}

// generateReviewData creates the JSON structure for AI review
func (p *parser) generateReviewData(txns []Txn) ReviewData {
	// Build enhanced category information with comments and examples
	allCategories := make([]CategoryInfo, 0, len(p.accounts))
	for _, accountName := range p.accounts {
		catInfo := CategoryInfo{
			Name:     accountName,
			Comment:  p.accountComments[accountName],
			Examples: p.selectDiverseExamples(accountName, 5),
		}
		allCategories = append(allCategories, catInfo)
	}

	reviewData := ReviewData{
		Transactions:  make([]ReviewTransaction, 0, len(txns)),
		AllCategories: allCategories,
	}

	for _, t := range txns {
		// Get Bayesian classifier predictions
		terms := prepareDescriptionForClassification(t.Desc)
		scores, _, _ := p.cl.LogScores(terms)

		// Create pairs of scores and positions
		type scorePair struct {
			score float64
			pos   int
		}
		pairs := make([]scorePair, 0, len(scores))
		for pos, score := range scores {
			pairs = append(pairs, scorePair{score, pos})
		}
		sort.Slice(pairs, func(i, j int) bool {
			return pairs[i].score > pairs[j].score
		})

		// Convert log scores to probabilities (normalize)
		// Using softmax-like normalization
		maxScore := pairs[0].score
		var sumExp float64
		expScores := make([]float64, len(pairs))
		for i, pr := range pairs {
			expScores[i] = math.Exp(pr.score - maxScore)
			sumExp += expScores[i]
		}

		// Build categories with normalized confidence scores
		categories := make([]CategoryScore, 0, 5)
		for i := range min(len(pairs), 5) {
			pr := pairs[i]
			confidence := expScores[i] / sumExp
			categories = append(categories, CategoryScore{
				Category:   string(p.classes[pr.pos]),
				Confidence: confidence,
			})
		}

		reviewTxn := ReviewTransaction{
			Date:        t.Date.Format("2006-01-02"),
			Description: t.Desc,
			Amount:      t.Cur,
			Currency:    t.CurName,
			Account:     t.From,
			Categories:  categories,
		}
		if t.Cur > 0 {
			reviewTxn.Account = t.To
		}
		reviewData.Transactions = append(reviewData.Transactions, reviewTxn)
	}

	return reviewData
}

// buildAIPrompt creates the prompt for Claude API
func buildAIPrompt(reviewData ReviewData) string {
	prompt := `You are a financial transaction categorization expert. Your task is to review transactions and categorize them accurately.

**Available Categories Context:**
The "all_categories" field contains detailed information about each available category:
- "name": The category account name (e.g., "Expenses:Food:Groceries")
- "comment": Human-written description from the ledger file explaining what this category is for
- "examples": Up to 3 diverse example transactions from historical data that were previously categorized here

Use this context to understand what types of transactions belong in each category. The examples show real patterns of spending, and the comments provide the user's intent for each category.

**Bayesian Classifier Context:**
Each transaction includes predictions from a Bayesian classifier trained on historical data. The "categories" field shows the top 5 predicted categories with confidence scores (0-1), sorted by confidence.

**IMPORTANT - Description Quality Assessment:**
Before trusting Bayesian predictions, evaluate the transaction description quality:
- CLEAR descriptions: Contain recognizable merchant names, specific services, or obvious category indicators (e.g., "WHOLE FOODS", "SHELL GAS", "NETFLIX")
- AMBIGUOUS descriptions: Generic terms, codes, abbreviations, or vague text that could apply to multiple categories (e.g., "PAYMENT", "PURCHASE 1234", "SQ *Unknown", "ACH TRANSFER")

**How to use Bayesian predictions:**
1. HIGH Bayesian confidence (>= 0.8) + CLEAR description:
   - The prediction is likely correct
   - Prefer to use it unless the description clearly indicates otherwise

2. HIGH Bayesian confidence (>= 0.8) + AMBIGUOUS description:
   - BE SKEPTICAL - the Bayesian classifier may be confidently wrong
   - Ignore the Bayesian prediction and analyze the description carefully
   - If you cannot determine the category with confidence, mark as uncertain

3. MEDIUM confidence (0.5-0.8):
   - Use as a suggestion only, regardless of description clarity
   - Do your own analysis based on the transaction details

4. LOW confidence (< 0.5):
   - The Bayesian prediction is unreliable
   - Do your own analysis based on the transaction description

**Decision Rules:**
1. First, evaluate if the transaction description is clear or ambiguous
2. Analyze the transaction description, amount, date, and Bayesian predictions
3. For high-confidence Bayesian predictions (>= 0.8) with CLEAR descriptions, prefer to use them
4. For high-confidence Bayesian predictions (>= 0.8) with AMBIGUOUS descriptions, be very skeptical and rely on your own analysis
5. ALWAYS generate up to 3 most likely category suggestions with confidence scores (0-1), sorted by confidence descending
6. If top suggestion confidence >= 0.7: source="ai", otherwise source="uncertain"
7. Keep reasoning BRIEF (5-15 words max). Format: "Clear/Ambiguous. Bayesian=X.XX. [Followed/Overrode]: reason"

**Output Format:**
Return a JSON object with your categorization decisions in the SAME ORDER as the input transactions:

{
  "decisions": [
    {
      "suggested_categories": [
        {"category": "Expenses:Food:Groceries", "confidence": 0.85},
        {"category": "Expenses:Food:Restaurant", "confidence": 0.10},
        {"category": "Expenses:Shopping", "confidence": 0.05}
      ],
      "source": "ai",
      "reasoning": "Clear. Bayesian=0.82. Followed."
    },
    {
      "suggested_categories": [
        {"category": "Expenses:TODO:Manual", "confidence": 0.45},
        {"category": "Expenses:Shopping", "confidence": 0.30},
        {"category": "Expenses:Food", "confidence": 0.25}
      ],
      "source": "uncertain",
      "reasoning": "Ambiguous. Bayesian=0.88. Cannot verify."
    }
  ]
}

**Rules:**
- Return decisions in the SAME ORDER as input transactions (array index corresponds to transaction)
- Each decision must have "suggested_categories" with 1-3 category suggestions, sorted by confidence descending
- Each suggestion has "category" (one of the available categories or "Expenses:TODO:Manual") and "confidence" (0-1)
- "source" is "ai" if top confidence >= 0.7, otherwise "uncertain"
- "reasoning" must be BRIEF (5-10 words): description quality, Bayesian confidence, and decision
- Confidence scores should sum to approximately 1.0 but don't need to be exact
- IMPORTANT: Return exactly one decision for each transaction in the input

**Transaction Data:**

`
	// Add transactions as JSON
	data, _ := json.MarshalIndent(reviewData, "", "  ")
	prompt += string(data)
	prompt += "\n\n**Now generate the JSON response with your categorization decisions:**"

	return prompt
}

// callClaudeAPI calls the Claude API to categorize transactions and returns decisions
func callClaudeAPI(apiKey string, model string, reviewData ReviewData, outputPath string, batchNum int) (AIResponse, error) {
	var emptyResponse AIResponse

	if len(apiKey) == 0 {
		return emptyResponse, fmt.Errorf("ANTHROPIC_API_KEY not set. Please set it in environment or config.yaml")
	}

	if len(model) == 0 {
		model = "claude-sonnet-4-5-20250929"
	}

	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	prompt := buildAIPrompt(reviewData)

	// Write request (prompt) to file for debugging
	debugDir := path.Dir(outputPath)
	if *debug {
		requestPath := path.Join(debugDir, fmt.Sprintf("%s%d.req.txt", debugPrefix, batchNum))
		if err := os.WriteFile(requestPath, []byte(prompt), 0o644); err != nil {
			fmt.Printf("Warning: Unable to write request to %s: %v\n", requestPath, err)
		} else {
			fmt.Printf("Request written to: %s\n", requestPath)
		}

		fmt.Printf("API Key: %s...\n", apiKey[:10])
		fmt.Printf("Model: %s\n", model)
		fmt.Printf("Prompt length: %d characters\n", len(prompt))
	}

	ctx := context.Background()
	message, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 8192,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return emptyResponse, fmt.Errorf("claude API call failed: %v", err)
	}

	// Extract the text content from the response
	if len(message.Content) == 0 {
		return emptyResponse, fmt.Errorf("empty response from Claude API")
	}

	var responseText string
	for _, block := range message.Content {
		if block.Type == "text" {
			responseText += block.Text
		}
	}

	// Write response to file for debugging
	if *debug {
		responsePath := path.Join(debugDir, fmt.Sprintf("%s%d.resp.txt", debugPrefix, batchNum))
		if err := os.WriteFile(responsePath, []byte(responseText), 0o644); err != nil {
			fmt.Printf("Warning: Unable to write response to %s: %v\n", responsePath, err)
		} else {
			fmt.Printf("Response written to: %s\n", responsePath)
		}
	}

	// Parse JSON response
	// Claude might wrap JSON in markdown code blocks, so extract it
	jsonStart := strings.Index(responseText, "{")
	jsonEnd := strings.LastIndex(responseText, "}")
	if jsonStart == -1 || jsonEnd == -1 {
		return emptyResponse, fmt.Errorf("no JSON found in response: %s", responseText)
	}
	jsonText := responseText[jsonStart : jsonEnd+1]

	var aiResponse AIResponse
	if err := json.Unmarshal([]byte(jsonText), &aiResponse); err != nil {
		return emptyResponse, fmt.Errorf("failed to parse JSON response: %v\nResponse: %s", err, jsonText)
	}

	return aiResponse, nil
}

// processAIReview handles the AI review workflow and returns all transactions for manual review
func (p *parser) processAIReview(txns []Txn, outputPath string, apiKey string, model string) ([]Txn, error) {
	// Split transactions into high-confidence (Bayesian) and low-confidence (needs AI review)
	confidenceThreshold := *bayesianThreshold
	var highConfidenceTxns []Txn
	var lowConfidenceTxns []Txn

	fmt.Printf("Analyzing %d transactions with Bayesian threshold %.2f...\n", len(txns), confidenceThreshold)

	for _, t := range txns {
		// Get Bayesian classifier prediction
		terms := prepareDescriptionForClassification(t.Desc)
		scores, _, _ := p.cl.LogScores(terms)

		// Find top score and normalize to confidence
		if len(scores) == 0 {
			lowConfidenceTxns = append(lowConfidenceTxns, t)
			continue
		}

		// Get max score for normalization
		maxScore := scores[0]
		for _, score := range scores {
			if score > maxScore {
				maxScore = score
			}
		}

		// Normalize scores using softmax
		var sumExp float64
		expScores := make([]float64, len(scores))
		for i, score := range scores {
			expScores[i] = math.Exp(score - maxScore)
			sumExp += expScores[i]
		}

		// Get top confidence
		topConfidence := expScores[0] / sumExp
		for _, exp := range expScores {
			conf := exp / sumExp
			if conf > topConfidence {
				topConfidence = conf
			}
		}

		if topConfidence >= confidenceThreshold {
			// High-confidence Bayesian prediction
			hits := p.topHits(t.Desc)
			if len(hits) > 0 {
				if t.Cur > 0 {
					t.From = string(hits[0])
				} else {
					t.To = string(hits[0])
				}
			}
			t.AIReason = fmt.Sprintf("Bayesian: confidence=%.2f", topConfidence)
			highConfidenceTxns = append(highConfidenceTxns, t)
		} else {
			lowConfidenceTxns = append(lowConfidenceTxns, t)
		}
	}

	fmt.Printf("High confidence Bayesian: %d transactions\n", len(highConfidenceTxns))
	fmt.Printf("Low confidence (needs AI review): %d transactions\n", len(lowConfidenceTxns))

	// All transactions to return for manual review
	var allTxns []Txn
	allTxns = append(allTxns, highConfidenceTxns...)

	// Process low-confidence transactions with Claude API
	if len(lowConfidenceTxns) > 0 {
		fmt.Printf("\nSending %d low-confidence transactions to Claude for review...\n", len(lowConfidenceTxns))

		// Batch size for API calls
		totalBatches := (len(lowConfidenceTxns) + *batchSize - 1) / *batchSize

		for batchNum := range totalBatches {
			start := batchNum * *batchSize
			end := min(start+*batchSize, len(lowConfidenceTxns))

			batch := lowConfidenceTxns[start:end]
			fmt.Printf("Processing batch %d/%d (%d transactions)...\n", batchNum+1, totalBatches, len(batch))

			// Generate review data for this batch
			reviewData := p.generateReviewData(batch)

			// Write review JSON for this batch (for debugging/inspection)
			if batchNum == 0 || *debug {
				batchReviewPath := fmt.Sprintf("%s.review.batch%d.json", outputPath, batchNum)
				if err := writeReviewJSONToPath(reviewData, batchReviewPath); err != nil {
					return nil, err
				}
			}

			// Call Claude API for this batch
			aiResponse, err := callClaudeAPI(apiKey, model, reviewData, outputPath, batchNum)
			if err != nil {
				return nil, fmt.Errorf("batch %d failed: %v", batchNum+1, err)
			}

			// Validate we got the right number of decisions
			assertf(len(aiResponse.Decisions) == len(batch),
				"Claude returned %d decisions for %d transactions in batch %d",
				len(aiResponse.Decisions), len(batch), batchNum+1)

			// Store AI decisions in transactions and add to allTxns
			for i, decision := range aiResponse.Decisions {
				t := batch[i]

				// Ensure we have at least one suggested category
				if len(decision.SuggestedCategories) == 0 {
					log.Printf("Warning: No suggested categories for transaction %d in batch %d", i, batchNum)
					continue
				}

				// Sort suggestions by confidence (descending) - should already be sorted from AI
				sort.Slice(decision.SuggestedCategories, func(i, j int) bool {
					return decision.SuggestedCategories[i].Confidence > decision.SuggestedCategories[j].Confidence
				})

				// Store AI suggestions for display in UI
				t.AISuggestions = decision.SuggestedCategories

				// Use top suggestion as the category
				topCategory := decision.SuggestedCategories[0]
				t.AIReason = fmt.Sprintf("AI: confidence=%.2f, %s", topCategory.Confidence, decision.Reasoning)

				// Apply the top category
				if t.Cur > 0 {
					t.From = topCategory.Category
				} else {
					t.To = topCategory.Category
				}
				allTxns = append(allTxns, t)
			}

			fmt.Printf("Batch %d/%d: %d transactions categorized by AI\n",
				batchNum+1, totalBatches, len(batch))
		}
	}

	fmt.Printf("\n✓ AI categorization completed!\n")
	fmt.Printf("  - Total transactions: %d\n", len(allTxns))
	fmt.Printf("  - Bayesian high-confidence: %d\n", len(highConfidenceTxns))
	fmt.Printf("  - AI-reviewed: %d\n", len(lowConfidenceTxns))
	fmt.Printf("\nAll transactions will be presented for manual review.\n")

	return allTxns, nil
}

// writeReviewJSONToPath writes review data to a specific path
func writeReviewJSONToPath(reviewData ReviewData, filePath string) error {
	data, err := json.MarshalIndent(reviewData, "", "  ")
	if err != nil {
		return fmt.Errorf("unable to marshal review data: %v", err)
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return fmt.Errorf("unable to write review file: %v", err)
	}
	if *debug {
		fmt.Printf("Review data written to: %s\n", filePath)
	}
	return nil
}

func (p *parser) removeDuplicates(txns []Txn) []Txn {
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

	allowed := time.Duration(*dupWithin) * time.Hour
	within := func(a, b time.Time) bool {
		dur := a.Sub(b)
		return math.Abs(float64(dur)) <= float64(allowed)
	}

	final := txns[:0]
	for _, t := range txns {
		var found bool
		tdesc := sanitize(t.Desc)
		for _, pr := range prev {
			if pr.Date.After(t.Date.Add(allowed)) {
				break
			}
			pdesc := sanitize(pr.Desc)
			if tdesc == pdesc && within(pr.Date, t.Date) && math.Abs(pr.Cur) == math.Abs(t.Cur) {
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
	errc("\tERROR: " + msg + " ")
	fmt.Println()
	fmt.Println("Flags available:")
	flag.PrintDefaults()
	fmt.Println()
}

func validateJournalSetup(journalPath string, data []byte) error {
	// Check if journal file exists
	if _, err := os.Stat(journalPath); os.IsNotExist(err) {
		// Create directory if it doesn't exist
		if err := os.MkdirAll(path.Dir(journalPath), 0o755); err != nil {
			return fmt.Errorf("unable to create directory for journal file: %v", err)
		}
		return fmt.Errorf("journal file does not exist")
	}

	// Try to run ledger csv command to see if the journal is valid
	cmd := exec.Command("ledger", "-f", journalPath, "csv")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("journal file is not valid or has no transactions: %v", err)
	}

	// Check if there are any transactions with categories
	if len(output) == 0 {
		return fmt.Errorf("journal file contains no transactions with categories")
	}

	// If data is provided, check if the journal has account declarations for basic categories
	if data != nil {
		dataStr := string(data)
		hasExpenses := strings.Contains(dataStr, "account Expenses")
		hasAssets := strings.Contains(dataStr, "account Assets")
		hasIncome := strings.Contains(dataStr, "account Income")

		if !hasExpenses && !hasAssets && !hasIncome {
			return fmt.Errorf("journal file lacks basic account categories (Assets, Income, Expenses)")
		}
	}

	return nil
}

func askUserToSetupJournal() bool {
	fmt.Println()
	fmt.Println("Your journal file appears to be empty or lacks basic account categories.")
	fmt.Println("Would you like me to create basic account categories in your journal? (Y/n)")
	fmt.Print("This will add common accounts like Assets, Income, and Expenses: ")

	var response string
	fmt.Scanln(&response)
	response = strings.ToLower(strings.TrimSpace(response))

	return response == "" || response == "y" || response == "yes"
}

func createBasicJournalSetup(journalPath string) error {
	basicSetup := `; Basic account declarations for into-ledger
; Created automatically - you can modify these as needed

account Assets:Checking
account Assets:Savings
account Assets:Cash

account Income:Salary
account Income:Interest
account Income:Other

; Broadly speaking, we'll narrow it down to 5 expense categories.
;
; Home   : Rent, Utils, Internet, Moves, Furniture, Phone.
; Food   : Grocery, Restaurant.
; Kids   : Education, After School.
; Travel : Car, Flight, Museums, Lyft/Uber, Cinema.
; Wants  : Cash, Gifts, Shopping, Upkeep, Gym, Online.
; Others : Fee, Medical, Docs, Mail, Donations.

account Expenses:Home
account Expenses:Food
account Expenses:Kids
account Expenses:Travel
account Expenses:Wants
account Expenses:Others
account Expenses:Small

account Liabilities:Credit

; Example transactions - you can remove these
2024/01/01 * Sample grocery purchase
    Expenses:Food               $25.00
    Assets:Checking

2024/01/02 * Sample gas purchase  
    Expenses:Travel             $40.00
    Assets:Checking

`

	// Check if file exists and has content
	data, err := os.ReadFile(journalPath)
	if err != nil {
		// File doesn't exist, create it with basic setup
		return os.WriteFile(journalPath, []byte(basicSetup), 0o644)
	}

	// If file is empty or very small, write the basic setup
	if len(data) < 10 {
		return os.WriteFile(journalPath, []byte(basicSetup), 0o644)
	}

	// If file has content, append the basic accounts
	file, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.WriteString("\n" + basicSetup)
	return err
}

func cleanupOldDebugFiles(outputPath string) {
	debugDir := path.Dir(outputPath)
	files, err := os.ReadDir(debugDir)
	if err != nil {
		return
	}

	var removed int
	for _, file := range files {
		if strings.HasPrefix(file.Name(), debugPrefix) {
			filePath := path.Join(debugDir, file.Name())
			if err := os.Remove(filePath); err == nil {
				removed++
			}
		}
	}

	if removed > 0 && *debug {
		fmt.Printf("Cleaned up %d old debug files\n", removed)
	}
}

func main() {
	flag.Parse()

	// Check if ledger is installed and available
	if _, err := exec.LookPath("ledger"); err != nil {
		oerr("ledger is not installed or not in PATH. Please install ledger from https://ledger-cli.org/")
		return
	}

	defer saneMode()
	singleCharMode()

	checkf(os.MkdirAll(*configDir, 0o755), "Unable to create directory: %v", *configDir)

	// Parse account flag: check if it's an integer (column index) or string (account name)
	accountColIdx := -1
	accountName := ""
	if len(*account) > 0 {
		if colIdx, err := strconv.Atoi(*account); err == nil {
			// It's an integer - column index for CSV
			accountColIdx = colIdx
			fmt.Printf("Using CSV column %d for account information\n", accountColIdx)
		} else {
			// It's a string - account name
			accountName = *account
			fmt.Printf("Using account: %s\n", accountName)
		}
	}

	configPath := path.Join(*configDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err == nil {
		var c configs
		checkf(yaml.Unmarshal(data, &c), "Unable to unmarshal yaml config at %v", configPath)
		if ac, has := c.Accounts[*account]; has {
			fmt.Printf("Using flags from config: %+v\n", ac)
			for k, v := range ac {
				flag.Set(k, v)
			}
		}
	}
	keyfile := path.Join(*configDir, *shortcuts)
	short = keys.ParseConfig(keyfile)
	setDefaultMappings(short)
	defer short.Persist(keyfile)

	if len(*journal) == 0 {
		oerr("Please specify the input ledger journal file")
		return
	}

	// Check if journal file has proper setup with categories
	if err := validateJournalSetup(*journal, nil); err != nil {
		fmt.Printf("Journal setup issue: %v\n", err)
		if askUserToSetupJournal() {
			if err := createBasicJournalSetup(*journal); err != nil {
				checkf(err, "Unable to create basic journal setup")
			}
		} else {
			return
		}
	}

	data, err = os.ReadFile(*journal)
	checkf(err, "Unable to read file: %v", *journal)
	alldata := includeAll(path.Dir(*journal), data)

	// Default output to journal file if not specified
	if len(*output) == 0 {
		*output = *journal
		fmt.Printf("Output file not specified, using journal file: %s\n", *output)
	}
	if _, err := os.Stat(*output); os.IsNotExist(err) {
		_, err := os.Create(*output)
		checkf(err, "Unable to check for output file: %v", *output)
	}

	// Clean up old debug files from previous runs
	cleanupOldDebugFiles(*output)

	tf, err := os.CreateTemp("", "ledger-csv-txns")
	checkf(err, "Unable to create temp file")
	defer os.Remove(tf.Name())

	db, err := bolt.Open(tf.Name(), 0o600, nil)
	checkf(err, "Unable to open boltdb at %v", tf.Name())
	defer db.Close()

	db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		checkf(err, "Unable to create default bucket in boltdb.")
		return nil
	})

	of, err := os.OpenFile(*output, os.O_APPEND|os.O_WRONLY, 0o600)
	checkf(err, "Unable to open output file: %v", *output)

	p := parser{data: alldata, db: db}
	p.parseAccounts()
	p.parseAccountMappings() // Parse account mappings from ledger comments
	p.parseTransactions()

	// Scanning done. Now train classifier.
	p.generateClasses()

	var txns []Txn
	if len(*csvFile) == 0 {
		oerr("Please specify a CSV file with the -csv flag")
		return
	}
	in, err := os.ReadFile(*csvFile)
	checkf(err, "Unable to read csv file: %v", *csvFile)
	txns = parseTransactionsFromCSV(in, accountColIdx)

	// Assign accounts to transactions
	for i := range txns {
		var ledgerAccount string

		// If account column was specified and we have an account value from CSV
		if accountColIdx >= 0 && len(txns[i].Account) > 0 {
			// Try to match CSV account to ledger account
			ledgerAccount = p.matchAccountToLedger(txns[i].Account)
			if len(ledgerAccount) == 0 {
				fmt.Printf("WARNING: Could not map CSV account '%s' to any ledger account. "+
					"Consider adding csv-account mappings to your ledger file.\n", txns[i].Account)
			}
		} else if len(accountName) > 0 {
			// Use the fixed account name from flag
			ledgerAccount = accountName
		}

		// If we couldn't determine the account, require it to be specified
		if len(ledgerAccount) == 0 {
			oerr("Unable to determine account for transaction. Please specify account with -a flag " +
				"(as account name or CSV column index with csv-account mappings in ledger file)")
			return
		}

		// Assign the ledger account to the transaction
		if txns[i].Cur > 0 {
			txns[i].To = ledgerAccount
		} else {
			txns[i].From = ledgerAccount
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

	// Apply transaction limit if specified
	if *limitTxns > 0 && len(txns) > *limitTxns {
		fmt.Printf("\n")
		color.New(color.BgYellow, color.FgBlack).Printf(" LIMIT ")
		fmt.Printf(" Processing %d of %d transactions (limit=%d)\n", *limitTxns, len(txns), *limitTxns)
		txns = txns[:*limitTxns]
	}

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

	// Check if AI review mode is enabled
	if *aiReview {
		// Get API key from environment or config
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		model := ""

		// Read config for AI settings if available
		configPath := path.Join(*configDir, "config.yaml")
		if configData, err := os.ReadFile(configPath); err == nil {
			var c configs
			if err := yaml.Unmarshal(configData, &c); err == nil {
				if len(c.AI.APIKey) > 0 {
					apiKey = c.AI.APIKey
				}
				if len(c.AI.Model) > 0 {
					model = c.AI.Model
				}
			}
		}

		// Process with AI and get all transactions for manual review
		reviewTxns, err := p.processAIReview(txns, *output, apiKey, model)
		if err != nil {
			log.Fatalf("AI review failed: %v", err)
		}

		// All AI-categorized transactions will be shown for manual review
		txns = reviewTxns

		fmt.Println("\n" + strings.Repeat("=", 70))
		fmt.Println("MANUAL REVIEW OF AI CATEGORIZATIONS")
		fmt.Println(strings.Repeat("=", 70) + "\n")
		fmt.Printf("All %d transactions are ready for your review.\n", len(txns))
		fmt.Println("AI reasoning is displayed for each transaction to help with your decision.")
		fmt.Println()
	}

	// Original interactive mode
	p.showAndCategorizeTxns(txns)

	final := p.iterateDB()
	sort.Sort(byTime(final))

	_, err = fmt.Fprintf(of, "; into-ledger run at %v\n\n", time.Now())
	checkf(err, "Unable to write into output file: %v", of.Name())

	for _, t := range final {
		if _, err := of.WriteString(ledgerFormat(t)); err != nil {
			log.Fatalf("Unable to write to output: %v", err)
		}
	}
	fmt.Printf("Transactions written to file: %s\n", of.Name())
	checkf(of.Close(), "Unable to close output file: %v", of.Name())
}

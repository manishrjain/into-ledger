package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/boltdb/bolt"
	"github.com/jbrukh/bayesian"
)

type parser struct {
	db       *bolt.DB
	data     []byte
	txns     []Txn
	classes  []bayesian.Class
	cl       *bayesian.Classifier
	accounts []string
}

func (p *parser) parseTransactions() {
	csvCommand := []string{"ledger", "-f", *journal, "csv"}
	if *ledgerOption != "" {
		csvCommand = append(csvCommand, *ledgerOption)
	}
	out, err := exec.Command(csvCommand[0], (csvCommand[1:])...).Output()
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

		existingAccounts.Add(t.To)
	}
}

func (p *parser) parseAccounts() {
	s := bufio.NewScanner(bytes.NewReader(p.data))
	var acc string
	for s.Scan() {
		m := racc.FindStringSubmatch(s.Text())
		if len(m) < 2 {
			continue
		}
		acc = m[1]
		if len(acc) == 0 {
			continue
		}
		// TODO p.accounts could be replaced by existingAccount perhaps?
		p.accounts = append(p.accounts, acc)
		existingAccounts.Add(acc)
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
		desc := strings.ToLower(t.Desc)
		p.cl.Learn(strings.Split(desc, " "), bayesian.Class(t.To))
	}
	p.cl.ConvertTermsFreqToTfIdf()
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

func parseTransactionsFromCSV(in []byte) []Txn {
	ignored := make(map[int]bool)
	if len(*ignore) > 0 {
		for _, i := range strings.Split(*ignore, ",") {
			pos, err := strconv.Atoi(i)
			checkf(err, "Unable to convert to integer: %v", i)
			ignored[pos] = true
		}
	}

	result := make([]Txn, 0, 100)
	r := csv.NewReader(bytes.NewReader(in))
	r.Comma = []rune(*comma)[0]
	var t Txn
	var skipped int
	for {
		t = Txn{Key: make([]byte, 16)}
		// Have a unique key for each transaction in CSV, so we can unique identify and
		// persist them as we modify their category.
		_, err := rand.Read(t.Key)
		cols, err := r.Read()
		if err == io.EOF {
			break
		}
		if (err != nil) && len(cols) == 0 {
			// TODO Make this configurable
			log.Println("Warning: Empty line dropped")
			continue
		}
		checkf(err, "Unable to read line: %v\ncolumns: %v", strings.Join(cols, ", "), cols)
		if *skip > skipped {
			skipped++
			continue
		}

		var picked []string
		for i, col := range cols {
			if ignored[i] {
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
			fmt.Printf("Selected CSV    : %#v\n", picked)
			fmt.Printf("Parsed Date     : %v\n", t.Date)
			fmt.Printf("Parsed Desc     : %v\n", t.Desc)
			fmt.Printf("Parsed Currency : %v\n", t.Cur)
			log.Fatalln("Please ensure that the above CSV contains ALL the 3 required fields.")
		}
	}
	return result
}

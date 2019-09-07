package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

// +gen set
type Payee = string

// TODO Make this asynchronous
func listPayee() PayeeSet {
	payees := runCommand("ledger", "-f", *journal, "payees")
	return NewPayeeSet(payees...)
}

/// Substitution from key to value of payee name
type PayeeSubstitutions map[string]string

func (ps *PayeeSubstitutions) Persist(path string) {
	data, err := yaml.Marshal(*ps)
	if err != nil {
		log.Fatalf("marshal payee substitutions: %v", err)
	}

	if err := ioutil.WriteFile(path, data, 0644); err != nil {
		log.Fatalf("While writing payee substitutions to file '%v': %v", path, err)
	}
}

// performPayeeSubstitution(txns, payeeSubstitutions, existingPayees) change
// (for each txn) payee that are keys of payeeSubstitutions to the ass. Display
// a fuzzy selection menu among existing payee not in existingPayees and
// without existing substitution
func performPayeeSubstitution(txns []Txn, subst PayeeSubstitutions,
	existingPayees *PayeeSet) {
	// Continously select with fuzzy menu, without asking
	fuzzyContinous := false
TxnLoop:
	for i := range txns {
		txn := &txns[i]
		payee := txn.Desc
		if !existingPayees.Contains(payee) {
			if replacement, has := subst[payee]; has {
				txn.Desc = replacement
			} else {
				answer := "f"
				if !fuzzyContinous {
					fmt.Printf("Unknown payee: '%v' ([F]uzzy select/Fuzzy select [a]ll/[i]gnore/ig[n]ore all): ", payee)
					b := make([]byte, 1)
					_, _ = os.Stdin.Read(b)
					fmt.Println()
					answer = strings.ToLower(string(b))
				}
				if answer == "a" {
					fuzzyContinous = true
					answer = "f"
				}
				switch answer {
				case "n":
					break TxnLoop
				case "i":
					continue TxnLoop
				default:
					fuzzySelectUpdateTxn(txn, subst, payee, existingPayees)
				}
			}
		}
	}
}

func fuzzySelectUpdateTxn(txn *Txn, subst PayeeSubstitutions, payee string,
	existingPayees *PayeeSet) {
	payees := fuzzySelect(existingPayees.ToSlice(), payee, strings.ToLower(payee))
	if len(payees) > 0 {
		replacement := payees[0]
		subst[payee] = replacement
		txn.Desc = replacement
	} else {
		fmt.Println("Nothing selected")
	}
}

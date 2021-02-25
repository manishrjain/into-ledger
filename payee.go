package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

// A Payee as defined in ledger (ledger-cli.org)
// +gen set
type Payee = string

// TODO Make this asynchronous
func listPayee() PayeeSet {
	ledgerAccountCmd := []string{"ledger", "-f", *journal, "payees"}
	fmt.Printf("Getting account list with: `%v`", ledgerAccountCmd)
	payees := runCommand(ledgerAccountCmd[0], ledgerAccountCmd[1:]...)
	return NewPayeeSet(payees...)
}

// PayeeSubstitutions holds all the replacement from source payee (the key in
// the map) to the desired payee name (the value in the map)
type PayeeSubstitutions map[string]string

// Persist saves the payee substitutions to a yaml file
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
		if existingPayees.Contains(payee) {
			continue
		}

		if replacement, has := subst[payee]; has {
			txn.Desc = replacement
		} else {
			answer := "f"
			if !fuzzyContinous {
				answer = askPayeeQuestion(fmt.Sprintf("Unknown payee: '%v' ([F]uzzy select/Fuzzy select [a]ll/[i]gnore/ig[n]ore all): ", payee), "fain", "f")
			}
			switch answer {
			case "n":
				break TxnLoop

			case "i":
				continue TxnLoop

			case "a":
				fuzzyContinous = true
				fallthrough
			case "f":
				fuzzySelectUpdateTxn(txn, subst, payee, existingPayees)
			}
		}
	}
}

// askPayeeQuestion prompts user with question and loops while it doesn’t
// get an answer in choices. Empty string or line-return goes to defaultChoice
func askPayeeQuestion(question string, choices string, defaultChoice string) string {
	var answer string
	for {
		fmt.Print(question)
		b := make([]byte, 1)
		_, _ = os.Stdin.Read(b)
		fmt.Println()
		answer = strings.ToLower(string(b))
		if answer == "" || answer == "\n" {
			return defaultChoice
		}
		for _, c := range choices {
			if string(c) == answer {
				return answer
			}
		}
	}
}

func fuzzySelectUpdateTxn(txn *Txn, subst PayeeSubstitutions, payee string,
	existingPayees *PayeeSet) {
	replacement := ""
	// payees is like []string{"user entered query", "result 1", "result 2", …}
	payees := fuzzySelect(Fzf{
		Items:       existingPayees.ToSlice(),
		Prompt:      payee,
		Query:       strings.ToLower(payee),
		ReturnQuery: true,
	})
	// If there were one or more result, we want to keep the first one, since
	// we can’t have multiple payees for a transaction
	if len(payees) > 1 {
		replacement = payees[1]
	} else {
		// Since fuzzySelect insert the query as first element, the slice
		// always has at least one element
		replacement = payees[0]
		fmt.Println("Nothing selected, remplacement:", replacement)
	}
	// We let the existing payee if there is nothing better to replace with
	if replacement != "" {
		fmt.Println("Replacing with:", replacement)
		subst[payee] = replacement
		txn.Desc = replacement
	} else {
		fmt.Println("Not replaced")
		fmt.Printf("%#v", payees)
	}
}

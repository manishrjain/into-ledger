package main

import (
	"fmt"
	"os"
	"strings"
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

// performPayeeSubstitution(txns, payeeSubstitutions, existingPayees) change
// (for each txn) payee that are keys of payeeSubstitutions to the ass. Display
// a fuzzy selection menu among existing payee not in existingPayees and
// without existing substitution
func performPayeeSubstitution(txns []Txn, subst PayeeSubstitutions,
	existingPayees *PayeeSet) {
TxnLoop:
	for i := range txns {
		txn := &txns[i]
		payee := txn.Desc
		if !existingPayees.Contains(payee) {
			if replacement, has := subst[payee]; has {
				txn.Desc = replacement
			} else {
				fmt.Printf("Unknown payee: '%v' ([F]uzzy select/[i]gnore/ignore [a]ll):", payee)
				b := make([]byte, 1)
				_, _ = os.Stdin.Read(b)
				fmt.Println()
				switch strings.ToLower(string(b)) {
				case "a":
					break TxnLoop
				case "i":
					continue TxnLoop
				default:
					payees := fuzzySelect(existingPayees.ToSlice(), payee, strings.ToLower(payee))
					if len(payees) > 0 {
						replacement := payees[0]
						subst[payee] = replacement
						txn.Desc = replacement
					} else {
						fmt.Println("Nothing selected")
					}
				}
			}
		}
	}
}

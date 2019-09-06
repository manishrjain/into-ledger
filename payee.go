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

// performPayeeTranslation(txns, payeeTranslations, existingPayees) change
// (for each txn) payee that are keys of payeeTranslations to the ass. Display
// a warning for payee not in existingPayees and without translation
func performPayeeTranslation(txns []Txn, payeeTranslations map[string]string,
	existingPayees *PayeeSet) {
TxnLoop:
	for i := range txns {
		txn := &txns[i]
		payee := txn.Desc
		if !existingPayees.Contains(payee) {
			if replacement, has := payeeTranslations[payee]; has {
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
						payeeTranslations[payee] = replacement
						txn.Desc = replacement
					} else {
						fmt.Println("Nothing selected")
					}
				}
			}
		}
	}
}

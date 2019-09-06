package main

import "fmt"

// +gen set
type Payee = string

// TODO Make this asynchronous
func listPayee() PayeeSet {
	payees := runCommand("ledger", "-f", *journal, "payee")
	return NewPayeeSet(payees...)
}

// performPayeeTranslation(txns, payeeTranslations, existingPayees) change
// (for each txn) payee that are keys of payeeTranslations to the ass. Display
// a warning for payee not in existingPayees and without translation
func performPayeeTranslation(txns []Txn, payeeTranslations map[string]string,
	existingPayees PayeeSet) {
	for i := range txns {
		txn := &txns[i]
		payee := txn.Desc
		if !existingPayees.Contains(payee) {
			if replacement, has := payeeTranslations[payee]; has {
				txn.Desc = replacement
			} else {
				fmt.Printf("Unknown payee: '%v'\n", payee)
				// TODO Add fzf selection here
			}
		}
	}
}

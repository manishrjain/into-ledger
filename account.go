package main

// +gen set
type Account = string

// fuzzySelectAccount select an account with an external fuzzy finder and puts
// it into the given transaction. Returns an empty string if there is no match
func fuzzySelectAccount(txn *Txn, existingAccounts *AccountSet) Account {
	accounts := fuzzySelect(existingAccounts.ToSlice(), "", "", false)
	if len(accounts) == 0 {
		return ""
	}
	// TODO Support multiple accounts?
	return accounts[0]
}

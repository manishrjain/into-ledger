package main

// An Account as defined in ledger (ledger-cli.org)
// +gen set
type Account = string

// fuzzySelectAccount select an account with an external fuzzy finder and puts
// it into the given transaction. Returns an empty string if there is no match
func fuzzySelectAccount(txn *Txn, existingAccounts *AccountSet) Account {
	accounts := fuzzySelect(Fzf{
		Items: existingAccounts.ToSlice(),
		MoreArgs: []string{
			"--preview", "ledger b --force-color {}; ledger r --force-color {}",
			"--preview-window", "right:60%",
		},
	})
	if len(accounts) == 0 {
		return ""
	}
	// TODO Support multiple accounts?
	return accounts[0]
}

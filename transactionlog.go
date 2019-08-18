package main

import (
	"strings"
	"text/template"
	"time"
)

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
	var b strings.Builder
	tmpl.Execute(&b, toTxnTemplate(t))
	return b.String()
}

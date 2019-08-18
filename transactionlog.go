package main

import (
	"strings"
	"text/template"
	"time"

	humanize "github.com/dustin/go-humanize"
	uuid "github.com/nu7hatch/gouuid"
)

/// Functions to expand capabilities of transaction templates
var funcMap = map[string]interface{}{
	"humanFloat": humanize.FormatFloat,
	"commaFloat": func(f float64) string {
		return humanize.FormatFloat("# ###,##", f)
	},
	"uuid": func() (string, error) {
		u4, err := uuid.NewV4()
		return u4.String(), err
	},
}

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

func newTransactionTemplate(txnTemplateString string) (*template.Template, error) {
	return template.New("transaction").Funcs(funcMap).Parse(txnTemplateString)
}

/// ledgerFormat formats a string for insertion into a ledger journal, using
/// provided template.
func ledgerFormat(t Txn, tmpl *template.Template) string {
	var b strings.Builder
	tmpl.Execute(&b, toTxnTemplate(t))
	return b.String()
}

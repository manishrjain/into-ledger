package main

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestLedgerFormat(t *testing.T) {
	const longForm = "Jan 2, 2006 (UTC)"
	txnDate, _ := time.Parse(longForm, "Feb 3, 2013 (UTC)")
	txnKey := []uint8{0x23, 0xe9, 0x6a, 0xe2, 0xef, 0x94, 0x4, 0xc2, 0x88, 0x6c, 0xdc, 0xba, 0x95, 0x8f, 0xe0, 0xd3}
	txn := Txn{Date: txnDate, Desc: "Payee", To: "Assets:Checking", From: "Expenses:Food", Cur: 15.83, CurName: "USD", Key: txnKey, skipClassification: false, Done: true}

	// Test default format is compatible with the format historically used
	t.Run("defaultFormatEqualsHistorical", func(t *testing.T) {
		// Historical implementation without template
		historical := func(t Txn) string {
			var b bytes.Buffer
			b.WriteString(fmt.Sprintf("%s\t%s\n", t.Date.Format(stamp), t.Desc))
			b.WriteString(fmt.Sprintf("\t%-20s\t%.2f%s\n", t.To, math.Abs(t.Cur), t.CurName))
			b.WriteString(fmt.Sprintf("\t%s\n\n", t.From))
			return b.String()
		}(txn)
		tmpl, err := newTransactionTemplate(defaultTxnTemplateString)
		if err != nil {
			t.Errorf("Error while parsing template, %v\n", err)
			panic(err)
		}
		defaultFormat := ledgerFormat(txn, tmpl)
		if historical != defaultFormat {
			t.Errorf("The default format doesn’t follow historical format, got %s, want %s\n", defaultFormat, historical)
		}
	})

	t.Run("customTemplate", func(t *testing.T) {
		customTemplate := "{{.Date.Format \"2006/01/02\"}} * {{.Payee}}\n    {{.To | printf \"%-20s\"}}      {{.Amount}} {{.Currency}}\n    {{.From}}\n\n"
		tmpl, err := newTransactionTemplate(customTemplate)
		if err != nil {
			t.Errorf("Error while parsing template, %v\n", err)
			panic(err)
		}
		want := "2013/02/03 * Payee\n    Assets:Checking           15.83 USD\n    Expenses:Food\n\n"
		got := ledgerFormat(txn, tmpl)
		if got != want {
			t.Errorf("The result is not expected (template not completely interpreted?), got %s, want %s\n", got, want)
		}
	})

	t.Run("customTemplateWithHumanNumber", func(t *testing.T) {
		customTemplate := "{{.Date.Format \"2006/01/02\"}} * {{.Payee}}\n    {{.To | printf \"%-20s\"}}      {{.Amount | commaFloat }} {{.Currency}}\n    {{.From}}    {{.Amount | humanFloat `#|###,###`}}\n\n"
		tmpl, err := newTransactionTemplate(customTemplate)
		if err != nil {
			t.Errorf("Error while parsing template, %v\n", err)
			panic(err)
		}
		want := "2013/02/03 * Payee\n    Assets:Checking           15,83 USD\n    Expenses:Food    15,830\n\n"
		got := ledgerFormat(txn, tmpl)
		if got != want {
			t.Errorf("The result is not expected (template not completely interpreted?), got %s, want %s\n", got, want)
		}
	})

	t.Run("customTemplateWithUUID", func(t *testing.T) {
		customTemplate := "{{uuid}}"
		tmpl, err := newTransactionTemplate(customTemplate)
		if err != nil {
			t.Errorf("Error while parsing template, %v\n", err)
			panic(err)
		}
		got := ledgerFormat(txn, tmpl)
		if got == "" {
			t.Errorf("The uuid function did not expand")
		}
	})
}

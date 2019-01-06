package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"sort"
	"time"

	yaml "gopkg.in/yaml.v2"
)

type PlaidTxn struct {
	Id        string   `json:"transaction_id"`
	AccountId string   `json:"account_id"`
	Amount    float64  `json:"amount"`
	Category  []string `json:"category"`
	Date      string   `json:"date"`
	Currency  string   `json:"iso_currency_code"`
	Desc      string   `json:"name"`
	Pending   bool     `json:"pending"`
}

type Balance struct {
	Available float64 `json:"available"`
	Current   float64 `json:"current"`
}

type PlaidAccount struct {
	Id   string  `json:"account_id"`
	Name string  `json:"name"`
	Type string  `json:"subtype"`
	Bal  Balance `json:"balances"`
	Mask string  `json:"mask"`
}

type PlaidResponse struct {
	Accounts []PlaidAccount `json:"accounts"`
	Txns     []PlaidTxn     `json:"transactions"`
	Total    int            `json:"total_transactions"`
}

type PlaidOptions struct {
	AccountIds []string `json:"account_ids"`
	Count      int      `json:"count"`
	Offset     int      `json:"offset"`
}

type PlaidRequest struct {
	Secret      string            `json:"secret" yaml:"secret"`
	ClientId    string            `json:"client_id" yaml:"client_id"`
	AccessToken string            `json:"access_token" yaml:"access_token"`
	Accounts    map[string]string `json:"-" yaml:"accounts"`
	StartDate   string            `json:"start_date"`
	EndDate     string            `json:"end_date"`
	Opt         PlaidOptions      `json:"options"`
}

var plaidDate = "2006-01-02"

func googleIt(preq PlaidRequest) (*PlaidResponse, error) {
	client := &http.Client{}
	data, err := json.Marshal(preq)
	if err != nil {
		return nil, err
	}
	if *debug {
		fmt.Printf("Request to plaid.com: %s\n", data)
	}
	buf := bytes.NewBuffer(data)
	req, err := http.NewRequest("POST", "https://development.plaid.com/transactions/get", buf)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if *debug {
		fmt.Printf("response: %s\n", data)
	}
	pp := &PlaidResponse{}
	if err := json.Unmarshal(data, pp); err != nil {
		return nil, err
	}
	return pp, nil
}

func BalanceHistory(account string) error {
	preq, err := newPlaidRequest(account)
	if err != nil {
		return err
	}
	preq.StartDate = *plaidSince
	preq.Opt.Count = 1
	pp, err := googleIt(*preq)
	if err != nil {
		return err
	}
	total := pp.Total
	balance := pp.Accounts[0].Bal.Current
	fmt.Printf("Balance now: %.2f. Txns: %d\n", balance, total)

	width := 500
	preq.Opt.Count = width
	uniq := make(map[string]PlaidTxn)
	for offset := 0; offset < total; {
		preq.Opt.Offset = offset
		fmt.Printf("Using offset: %d\n", offset)

		pp, err := googleIt(*preq)
		if err != nil {
			return err
		}
		if len(pp.Accounts) == 0 {
			return fmt.Errorf("No account received for request: %+v\n", preq)
		}

		if *debug {
			fmt.Printf("first txn: %+v\n", pp.Txns[0].Id)
			fmt.Printf("last txn: %+v\n", pp.Txns[len(pp.Txns)-1].Id)
		}

		var last string
		var ot int
		for i, txn := range pp.Txns {
			if txn.Pending {
				continue
			}
			assertf(txn.AccountId == preq.Opt.AccountIds[0], "Account mismatch")
			if last != txn.Date {
				last = txn.Date
				ot = i // Set offset to date boundaries.
			}
			uniq[txn.Date+txn.Id] = txn
		}
		if len(pp.Txns) == width {
			offset = preq.Opt.Offset + ot
		} else {
			break
		}
	}

	var txns []PlaidTxn
	for _, txn := range uniq {
		txns = append(txns, txn)
	}
	sort.Slice(txns, func(i, j int) bool {
		return txns[i].Date > txns[j].Date
	})
	fmt.Printf("Latest: %+v\n", txns[0])
	fmt.Printf("Earliest: %+v\n", txns[len(txns)-1])

	var amts []float64
	sum := func() float64 {
		var t float64
		for _, a := range amts {
			t += a
		}
		return t
	}

	curDate := preq.EndDate
	for _, txn := range txns {
		if txn.Date != curDate {
			sort.Float64s(amts)
			fmt.Printf("%s : %8.2f. Amts: %8.2f | %+v\n", curDate, balance, sum(), amts)
			curDate = txn.Date
			amts = amts[:0]
		}
		balance -= txn.Amount
		amts = append(amts, txn.Amount)
	}
	fmt.Printf("%s : %8.2f. Amts: %8.2f | %+v\n", curDate, balance, sum(), amts)
	return nil
}

func newPlaidRequest(account string) (*PlaidRequest, error) {
	configPath := path.Join(*configDir, "plaid.yaml")
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	if *debug {
		fmt.Printf("data: %s\n", data)
	}

	preq := &PlaidRequest{}
	checkf(yaml.Unmarshal(data, preq), "Unable to parse plaid.yaml at %s", configPath)
	preq.StartDate = *plaidSince
	preq.EndDate = *plaidTo

	var accountId string
	for short, id := range preq.Accounts {
		if account == short {
			accountId = id
		}
	}
	if len(accountId) == 0 {
		return nil, fmt.Errorf("No account %q was found in config\n", accountId)
	}
	preq.Opt.AccountIds = []string{accountId}
	preq.Opt.Count = 500
	return preq, nil
}

func GetPlaidTransactions(account string) ([]Txn, error) {
	preq, err := newPlaidRequest(account)
	if err != nil {
		return nil, err
	}
	accountId := preq.Opt.AccountIds[0]

	var gotTxns int
	var txns []Txn
	for {
		pp, err := googleIt(*preq)
		if err != nil {
			return nil, err
		}

		var found bool
		for _, a := range pp.Accounts {
			if a.Id == accountId {
				fmt.Printf("Found account %+v\n", a)
				fmt.Printf("Balance: %+v\n", a.Bal)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("Unable to find any account with id: %q", accountId)
		}

		fmt.Println()
		for _, txn := range pp.Txns {
			if txn.Pending || txn.AccountId != accountId {
				continue
			}
			tm, err := time.Parse(plaidDate, txn.Date)
			if err != nil {
				return nil, err
			}
			t := Txn{
				Date:    tm,
				Desc:    txn.Desc,
				Cur:     -txn.Amount, // Negative because of how Ledger works.
				CurName: txn.Currency,
				Key:     []byte(txn.Id),
			}
			txns = append(txns, t)
			if *debug {
				fmt.Printf("Txn: %+v\n", txn)
			}
		}
		gotTxns += len(pp.Txns)
		fmt.Printf("Txns retrieved: %d. Total: %d.\n", gotTxns, pp.Total)
		if gotTxns < pp.Total {
			preq.Opt.Offset = gotTxns
		} else {
			break
		}
	}
	return txns, nil
}

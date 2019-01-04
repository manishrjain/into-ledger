package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
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

func GetPlaidTransactions(account string) ([]Txn, error) {
	configPath := path.Join(*configDir, "plaid.yaml")
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	fmt.Printf("data: %s\n", data)

	var preq PlaidRequest
	checkf(yaml.Unmarshal(data, &preq), "Unable to parse plaid.yaml at %s", configPath)
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

	client := &http.Client{}
	data, err = json.Marshal(preq)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Request to plaid.com: %s\n", data)
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

	fmt.Printf("data: %s\n", data)
	pp := &PlaidResponse{}
	if err := json.Unmarshal(data, pp); err != nil {
		return nil, err
	}

	var found bool
	for _, a := range pp.Accounts {
		if a.Id == accountId {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("Unable to find any account with id: %q", accountId)
	}
	fmt.Printf("Found account %q with id: %q", account, accountId)

	fmt.Println()
	var txns []Txn
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
			Cur:     txn.Amount,
			CurName: txn.Currency,
			Key:     []byte(txn.Id),
		}
		txns = append(txns, t)
		fmt.Printf("Txn: %+v\n", txn)
	}
	return txns, nil
}

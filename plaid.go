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

type PlaidRequest struct {
	Secret      string            `json:"secret" yaml:"secret"`
	ClientId    string            `json:"client_id" yaml:"client_id"`
	AccessToken string            `json:"access_token" yaml:"access_token"`
	Accounts    map[string]string `json:"-" yaml:"accounts"`
	StartDate   string            `json:"start_date"`
	EndDate     string            `json:"end_date"`
}

var plaidDate = "2006-01-02"

func GetPlaidTransactions() error {
	configPath := path.Join(*configDir, "plaid.yaml")
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return err
	}

	fmt.Printf("data: %s\n", data)

	var preq PlaidRequest
	checkf(yaml.Unmarshal(data, &preq), "Unable to parse plaid.yaml at %s", configPath)

	now := time.Now()
	preq.StartDate = now.Add(-24 * time.Hour).Format(plaidDate)
	preq.EndDate = now.Format(plaidDate)

	client := &http.Client{}
	data, err = json.Marshal(preq)
	if err != nil {
		return err
	}
	buf := bytes.NewBuffer(data)
	req, err := http.NewRequest("POST", "https://development.plaid.com/transactions/get", buf)
	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	fmt.Printf("data: %s\n", data)
	pp := &PlaidResponse{}
	if err := json.Unmarshal(data, pp); err != nil {
		return err
	}

	for _, a := range pp.Accounts {
		if _, has := preq.Accounts[a.Id]; has {
			fmt.Printf("Valid account: %+v\n", a)
		} else {
			fmt.Printf("Invalid account: %+v\n", a)
		}
	}
	fmt.Println()
	for _, txn := range pp.Txns {
		name, ok := preq.Accounts[txn.AccountId]
		if !ok {
			continue
		}
		fmt.Printf("Account: %+v Txn: %+v\n", name, txn)
	}
	return nil
}

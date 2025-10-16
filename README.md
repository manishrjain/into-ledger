into-ledger
-----------
into-ledger helps categorization of CSV transactions and conversion into ledger format for consumption by [ledger-cli.org](http://ledger-cli.org/). It makes importing hundreds of transactions into ledger a breeze. I typically get close to a hundred transactions per account per month myself, which is why I wrote this tool.

Features:
- *Accurate*             : Uses a much more accurate tf-idf expense classifier than used by cantino/reckon.
- *Includes and Aliases* : Correctly parses your existing journal file, handling all includes and account aliases.
- *Keyboard Shortcuts*   : Assigns dynamic keyboard shortcuts, so classifying transactions is just a keystroke away.
- *Auto save*            : Uses temporary storage (boltdb) to persist transactions that you have categorized or acknowledged to be correctly categorized, so you can quit whenever you want, without the risk of losing the work done so far.
- *Deduplication*        : Deduplicates incoming transactions from CSV against the transactions already present in ledger journal. This allows an easy resume from a broken workflow.
- *Nice UI*              : Colors and formatting, because it's not just about getting things done. It's also about making them look nice!


Install
-------

`go get -v -u github.com/manishrjain/into-ledger`


Help
----
```
Usage of ./into-ledger:
  -a string
    	Name of bank account transactions belong to.
  -c string
    	Set currency if any.
  -conf string
    	Config directory to store various into-ledger configs in. (default "/home/mrjn/.into-ledger")
  -csv string
    	File path of CSV file containing new transactions.
  -d string
    	Express your date format in numeric form w.r.t. Jan 02, 2006, separated by slashes (/). See: https://golang.org/pkg/time/ (default "01/02/2006")
  -debug
    	Additional debug information if set.
  -ic string
    	Comma separated list of columns to ignore in CSV.
  -j string
    	Existing journal to learn from.
  -o string
    	Journal file to write to. (default "out.ldg")
  -s int
    	Number of header lines in CSV to skip
```

Usage
-----

```
# Importing from Citibank Australia
$ into-ledger -j ~/ledger/journal.ldg -csv ~/ledger/ACCT_464_25_07_2016.csv --ic "3,4" -o out.data -a citi -c AUD -d "02/01/2006"

# Importing from Chase USA. Skips the first line in CSV file. Also skips the first (0) and second (1) column in csv. Outputs to out.data. Sets currency as USD.
$ into-ledger -j ~/ledger/journal.ldg -csv ~/ledger/Activity.CSV --ic "0,1" -o out.data -a chase -c USD -s 1
```

Having to specify these command line arguments over and over again is annoying. So, instead you can create a config file in "$HOME/.into-ledger/config.yaml", storing the flag values for reuse, like so:

```
accounts:
  chase:
    c: USD
    j: /home/mrjn/ledger/journal.ldg
    d: 01/02/2006
    ic: "0,1"
    o: /home/mrjn/ledger/chase.out
    s: 1
  cba-smart:
    c: AUD
    j: /home/mrjn/ledger/journal.ldg
    d: 02/01/2006
    ic: "3"
    o: /home/mrjn/ledger/cba.out
```

**Note: The way config is stored has changed recently. Please update your version of into-ledger using `go get -u -v github.com/manishrjain/into-ledger`. Also, update your config file.**

Now you can just run:
`into-ledger -a chase -csv <input-csv>`, or `into-ledger -a cba-smart -csv <input-csv>`

Account Mapping from CSV
-------------------------

If your CSV file contains an "Account" column (common in bank exports with multiple accounts), you can use `into-ledger` to automatically detect which account each transaction belongs to, instead of specifying a fixed account name.

### How to Use

The `-a` flag accepts either:
- **Account name** (string): e.g., `-a "Assets:Checking"` (traditional usage)
- **CSV column index** (integer): e.g., `-a 6` (new feature)

When using a column index, you need to add `csv-account` mappings to your ledger file to map CSV account identifiers to ledger accounts.

### Setting up Account Mappings

In your ledger journal file, add special comments under each account declaration:

```ledger
account Assets:Checking
  ; csv-account: CHK-1234
  ; csv-account: Main Checking
  ; csv-account: checking

account Liabilities:CreditCard
  ; csv-account: CC-5678
  ; csv-account: Visa Card
  ; csv-account: credit card

account Assets:Savings
  ; csv-account: SAV-9012
  ; csv-account: High Yield Savings
```

The matching is **case-insensitive** and uses **substring matching**. For example, if your CSV contains `"Bank Name - Main Checking (CHK-1234)"`, it will match the `CHK-1234` identifier and map to `Assets:Checking`.

### Example Usage

```bash
# CSV has account in column 6
$ into-ledger -j ~/ledger/journal.ldg -csv ~/Downloads/transactions.csv -a 6 -sc 0,1,5 -s 1 -d "2006-01-02"
```

This will:
1. Read the account name from column 6 of your CSV
2. Match it against the `csv-account` mappings in your ledger file
3. Automatically assign the correct ledger account to each transaction
4. Warn you if any CSV account couldn't be mapped

Dates
-----

into-ledger requires you to specify the date format in numeric form w.r.t. Jan 02, 2006. This is how Go language parses dates. This is frequently a cause of confusion among folks unfamiliar with the language. So, please find here the commonly used date formats and how to specify them in into-ledger.

| Formatting Style | Regions used in | into-ledger format |
| ---------------- | --------------- | ------------------ |
| Month/Day/Year | USA | 01/02/2006 |
| Month-Day-Year | USA | 01-02-2006 |
| Day/Month/Year | Australia, others | 02/01/2006 |
| Day-Month-Year | Australia, others | 02-01-2006 |
| Year/Month/Day | Ledger | 2006/01/02 |
| Year-Month-Day | Ledger | 2006-01-02 |


Keyboard Shortcuts
------------------

One of the great advantages of using `into-ledger` is how quickly you can categorize a transaction. Most of the times the underlying categorization algorithm is smart enough to do the right thing for you. However, for the rest, `into-ledger` shows you keyboard shortcuts to pick the right category.

`into-ledger` uses a keys module I wrote, which automatically assigns shortcuts to categories and persists them in `~/.into-ledger/shortcuts.yaml`. However, you might want to use certain keys for certain categories. In that case, feel free to hand-edit the `shortcuts.yaml` file. Just ensure that the same shortcut isn't being used twice in the file.

**Tip:** If you want to assign a shortcut to a category, but it's being used by another category, feel free to delete that category block from the shortcuts file. into-ledger will automatically reassign a new shortcut to the deleted category, and write it back.


Screenshots
-----------

**Parse transactions from CSV, and show automatically picked categories to be reviewed.**

![list of transactions](list.png)

**Detect duplicates transactions in CSV, which are already present in ledger journal.**

![duplicate detection](duplicates.png)

**Categorize transaction using persistent and dynamic keyboard shortcuts.**

![categorize transaction](txn.png)

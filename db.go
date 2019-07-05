package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log"
	"math"

	"github.com/boltdb/bolt"
)

func (p *parser) writeToDB(t Txn) {
	if err := p.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		var val bytes.Buffer
		enc := gob.NewEncoder(&val)
		checkf(enc.Encode(t), "Unable to encode txn: %v", t)
		return b.Put(t.Key, val.Bytes())

	}); err != nil {
		log.Fatalf("Write to db failed with error: %v", err)
	}
}

func (p *parser) iterateDB() []Txn {
	var txns []Txn
	if err := p.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t Txn
			dec := gob.NewDecoder(bytes.NewBuffer(v))
			if err := dec.Decode(&t); err != nil {
				log.Fatalf("Unable to parse txn from value of length: %v. Error: %v", len(v), err)
			}
			txns = append(txns, t)
		}
		return nil
	}); err != nil {
		log.Fatalf("Iterate over db failed with error: %v", err)
	}
	return txns
}

func (p *parser) categorizeBelow(txns []Txn) []Txn {
	unmatched := txns[:0]
	var count int
	var total float64
	for i := range txns {
		txn := &txns[i]
		if txn.Cur < 0 && txn.Cur >= -(*smallBelow) {
			total += txn.Cur
			count++
			txn.To = "Expenses:Small"
			printSummary(*txn, count, count)
			p.writeToDB(*txn)
		} else {
			unmatched = append(unmatched, *txn)
		}
	}
	fmt.Printf("\t%d txns totaling %.2f below %.2f have been categorized as 'Expenses:Small'.\n\n",
		count, math.Abs(total), *smallBelow)
	return unmatched
}

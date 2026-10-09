package helpers

import "github.com/easykafka/easykafka-go/publish"

// PublishInvoice is a value type for the publish package's encoder and topic
// tests. Its fields cover what makes JSON encoders differ: a nil slice, a map,
// an optional field and text that needs HTML escaping.
type PublishInvoice struct {
	ID       string            `json:"id"`
	Amount   float64           `json:"amount"`
	Lines    []string          `json:"lines"`
	Tags     map[string]string `json:"tags"`
	Note     string            `json:"note,omitempty"`
	Comment  string            `json:"comment"`
	Internal string            `json:"-"`
}

// NewPublishInvoice returns an invoice whose fields exercise every case above.
func NewPublishInvoice() PublishInvoice {
	return PublishInvoice{
		ID:       "INV-1",
		Amount:   12.5,
		Lines:    nil,
		Tags:     map[string]string{"b": "2", "a": "1"},
		Comment:  "<b>&</b>",
		Internal: "not encoded",
	}
}

// PublishInvoiceTopic returns a valid topic for PublishInvoice values, with one
// static header.
func PublishInvoiceTopic() publish.Topic[string, PublishInvoice] {
	return publish.Topic[string, PublishInvoice]{
		Name:        "invoices",
		EncodeKey:   publish.StringKey,
		EncodeValue: publish.JSONValue[PublishInvoice],
		Headers:     []publish.Header{{Key: "__TypeId__", Value: []byte("Invoice")}},
	}
}

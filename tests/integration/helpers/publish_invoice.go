package helpers

import "github.com/easykafka/easykafka-go/publish"

// PublishInvoice is a value type for the publisher's integration tests. Its
// fields cover what makes JSON encoders differ: a nil slice, a map and text
// that needs HTML escaping.
type PublishInvoice struct {
	ID      string            `json:"id"`
	Amount  float64           `json:"amount"`
	Lines   []string          `json:"lines"`
	Tags    map[string]string `json:"tags"`
	Comment string            `json:"comment"`
}

// NewPublishInvoice returns an invoice whose fields exercise every case above.
func NewPublishInvoice(id string) PublishInvoice {
	return PublishInvoice{
		ID:      id,
		Amount:  12.5,
		Tags:    map[string]string{"b": "2", "a": "1"},
		Comment: "<b>&</b>",
	}
}

// PublishInvoiceTopic returns a topic of PublishInvoice values keyed by
// string, with one static header, as a Spring consumer's type header is set.
func PublishInvoiceTopic(name string) publish.Topic[string, PublishInvoice] {
	return publish.Topic[string, PublishInvoice]{
		Name:        name,
		EncodeKey:   publish.StringKey,
		EncodeValue: publish.JSONValue[PublishInvoice],
		Headers:     []publish.Header{{Key: "__TypeId__", Value: []byte("Invoice")}},
	}
}

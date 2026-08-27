// Package kafka carries ValueContext across Kafka message headers.
// It depends on no Kafka client library: it exposes a Carrier interface
// (Get/Set/Keys) and helpers that any client can adapt to.
package kafka

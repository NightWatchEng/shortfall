// Package amqp carries ValueContext across AMQP message headers.
// It depends on no AMQP client library: it exposes a Carrier interface
// (Get/Set/Keys) and helpers that any client can adapt to.
package amqp

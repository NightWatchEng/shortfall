// Package sqs carries ValueContext across SQS message attributes.
// It depends on no AWS SDK: it exposes a Carrier interface (Get/Set/Keys)
// and helpers that any client can adapt to.
package sqs

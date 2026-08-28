module github.com/NightWatchEng/shortfall/adapters/payment/stripe

go 1.25.0

require github.com/NightWatchEng/shortfall v0.1.0

require golang.org/x/net v0.58.0 // indirect

require (
	github.com/stripe/stripe-go/v79 v79.12.0
	go.opentelemetry.io/otel v1.46.0 // indirect
)

replace github.com/NightWatchEng/shortfall => ../../..

module github.com/NightWatchEng/shortfall/adapters/export/loki

go 1.25.0

require (
	github.com/NightWatchEng/shortfall v0.1.0
	github.com/NightWatchEng/shortfall/adapters/httpbatch v0.0.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/NightWatchEng/shortfall => ../../..

replace github.com/NightWatchEng/shortfall/adapters/httpbatch => ../../httpbatch

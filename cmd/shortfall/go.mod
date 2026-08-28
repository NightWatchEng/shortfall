module github.com/NightWatchEng/shortfall/cmd/shortfall

go 1.25.0

require (
	github.com/NightWatchEng/shortfall v0.1.0
	github.com/NightWatchEng/shortfall/adapters/query/promql v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/sql v0.0.0
	modernc.org/sqlite v1.57.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/NightWatchEng/shortfall => ../..

replace github.com/NightWatchEng/shortfall/adapters/query/promql => ../../adapters/query/promql

replace github.com/NightWatchEng/shortfall/adapters/query/sql => ../../adapters/query/sql

module github.com/NightWatchEng/shortfall/test/loggolden

go 1.25.0

require (
	github.com/NightWatchEng/shortfall v0.1.0
	github.com/NightWatchEng/shortfall/adapters/export/cloudwatch v0.0.0
	github.com/NightWatchEng/shortfall/adapters/export/loki v0.0.0
	github.com/NightWatchEng/shortfall/adapters/export/splunkhec v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/cwinsights v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/logql v0.0.0
	github.com/NightWatchEng/shortfall/adapters/query/spl v0.0.0
)

require (
	github.com/NightWatchEng/shortfall/adapters/httpbatch v0.0.0 // indirect
	github.com/aws/aws-sdk-go-v2 v1.45.0 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.0 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.69.0 // indirect
	github.com/aws/smithy-go v1.28.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/NightWatchEng/shortfall => ../..
	github.com/NightWatchEng/shortfall/adapters/export/cloudwatch => ../../adapters/export/cloudwatch
	github.com/NightWatchEng/shortfall/adapters/export/loki => ../../adapters/export/loki
	github.com/NightWatchEng/shortfall/adapters/export/splunkhec => ../../adapters/export/splunkhec
	github.com/NightWatchEng/shortfall/adapters/httpbatch => ../../adapters/httpbatch
	github.com/NightWatchEng/shortfall/adapters/query/cwinsights => ../../adapters/query/cwinsights
	github.com/NightWatchEng/shortfall/adapters/query/logql => ../../adapters/query/logql
	github.com/NightWatchEng/shortfall/adapters/query/spl => ../../adapters/query/spl
)

# fixtures — deliberately broken fences the checker must reject

```go
x := 1

func f() {}
```

```go
y := 2
```

```yaml
version: 1
segments: [smb]
flows:
  broken.flow:
    money: { kind: fee }
    stages:
      - { name: auth, signals: ["http:POST /pay"] }
    baseline:
      seasonality: hour_of_week
      lookback_weeks: 8
    recovery:
      model: usage_loss_curve
      recovered_fraction: 0.6
      within: PT2H
```

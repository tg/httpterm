# httpterm

Extension to Go `http.Server` allowing for:

- Graceful shutdown
- Fine-tuning read timeouts

[![GoDoc](https://godoc.org/github.com/tg/httpterm?status.svg)](https://godoc.org/github.com/tg/httpterm)

## Examples
Run default server (as would `http.ListenAndServe`), exit gracefully on signal:

```go
package main

import (
    "fmt"

    "github.com/tg/httpterm"
)

func main() {
    s := httpterm.Server{CloseOnSignal: true}
    pending, err := s.ListenAndServe()

    fmt.Print(err) // will be nil if close has been triggered
    <-pending      // wait for pending requests (channel will be closed)
}
```

Run server on port 8080, shut down from the handler:

```go
package main

import (
    "fmt"
    "net/http"

    "github.com/tg/httpterm"
)

func main() {
    var s httpterm.Server
    s.Addr = ":8080"

    http.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
        s.Close()
    })

    pending, err := s.ListenAndServe()
    if err != nil {
        fmt.Println(err)
    }
    <-pending
}
```

Run server, read headers within 5 seconds, read body (usually POST content) within a minute, allow for idle connections for up to 15 seconds:
```go
package main

import (
    "fmt"
    "time"

    "github.com/tg/httpterm"
)

func main() {
    var s httpterm.Server
    s.Addr = ":8080"
    s.CloseOnSignal = true

    s.HeadReadTimeout = 5 * time.Second
    s.BodyReadTimeout = time.Minute
    s.IdleTimeout = 15 * time.Second

    pending, err := s.ListenAndServe()
    if err != nil {
        fmt.Print(err)
    }
    <-pending
}

```

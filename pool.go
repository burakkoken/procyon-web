package web

import "sync"

var (
	httpRequestPool           sync.Pool
	httpResponsePool          sync.Pool
	webTransactionContextPool sync.Pool
)

func initHttpRequestPool() {
	httpRequestPool = sync.Pool{
		New: newHttpRequest,
	}
}

func initHttpResponsePool() {
	httpResponsePool = sync.Pool{
		New: newHttpResponse,
	}
}

func initWebTransactionContextPool() {
	webTransactionContextPool = sync.Pool{
		New: newWebTransactionContext,
	}
}

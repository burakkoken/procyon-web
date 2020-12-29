package web

import (
	"encoding/xml"
	json "github.com/json-iterator/go"
	"github.com/procyon-projects/goo"
	configure "github.com/procyon-projects/procyon-configure"
	"github.com/procyon-projects/procyon-context"
	core "github.com/procyon-projects/procyon-core"
	peas "github.com/procyon-projects/procyon-peas"
	"github.com/valyala/fasthttp"
	"net/http"
	"reflect"
	"strconv"
)

type ProcyonServerApplicationContext struct {
	*context.BaseApplicationContext
	server Server
}

func NewProcyonServerApplicationContext(appId context.ApplicationId, contextId context.ContextId) *ProcyonServerApplicationContext {
	ctx := &ProcyonServerApplicationContext{}
	applicationContext := context.NewBaseApplicationContext(appId, contextId, ctx)
	ctx.BaseApplicationContext = applicationContext
	return ctx
}

func (ctx *ProcyonServerApplicationContext) GetWebServer() Server {
	return ctx.server
}

func (ctx *ProcyonServerApplicationContext) Configure() {
	ctx.BaseApplicationContext.Configure()
}

func (ctx *ProcyonServerApplicationContext) OnConfigure() {
	ctx.initializeInterceptors()
	_ = ctx.createWebServer()
}

func (ctx *ProcyonServerApplicationContext) initializeInterceptors() {
	peaFactory := ctx.BaseApplicationContext.GetPeaFactory()
	peaDefinitionRegistry := peaFactory.(peas.PeaDefinitionRegistry)
	peaNames := peaDefinitionRegistry.GetPeaDefinitionNames()

	for _, peaName := range peaNames {
		peaDefinition := peaDefinitionRegistry.GetPeaDefinition(peaName)
		if peaDefinition != nil && !ctx.isHandlerInterceptor(peaDefinition.GetPeaType()) {
			continue
		}
		peaFactory.GetPea(peaName)
	}
}

func (ctx *ProcyonServerApplicationContext) isHandlerInterceptor(typ goo.Type) bool {
	peaType := typ
	if peaType.IsFunction() {
		peaType = peaType.ToFunctionType().GetFunctionReturnTypes()[0]
	}

	if peaType.IsStruct() {
		structType := peaType.ToStructType()
		if structType.Implements(goo.GetType((*HandlerInterceptorBefore)(nil)).ToInterfaceType()) {
			return true
		} else if structType.Implements(goo.GetType((*HandlerInterceptorAfter)(nil)).ToInterfaceType()) {
			return true
		} else if structType.Implements(goo.GetType((*HandlerInterceptorAfterCompletion)(nil)).ToInterfaceType()) {
			return true
		}
	}
	return false
}

func (ctx *ProcyonServerApplicationContext) FinishConfigure() {
	logger := ctx.GetLogger()
	startedChannel := make(chan bool, 1)
	go func() {
		serverProperties := ctx.GetSharedPeaType(goo.GetType((*configure.WebServerProperties)(nil)))
		ctx.server.SetProperties(serverProperties.(*configure.WebServerProperties))
		logger.Info(ctx, "Procyon started on port(s): "+strconv.Itoa(int(ctx.GetWebServer().GetPort())))
		startedChannel <- true
		ctx.server.Run()
	}()
	<-startedChannel
}

func (ctx *ProcyonServerApplicationContext) createWebServer() error {
	ctx.server = newProcyonWebServer(ctx.BaseApplicationContext)
	return nil
}

type PathVariable struct {
	Key   string
	Value string
}

type WebRequestContext struct {
	// context
	contextIdBuffer        [36]byte
	contextIdStr           string
	fastHttpRequestContext *fasthttp.RequestCtx
	// cache
	path []byte
	args *fasthttp.Args
	uri  *fasthttp.URI
	// handler
	handlerChain *HandlerChain
	handlerIndex int
	// path variables
	pathVariables     [20]string
	pathVariableCount int
	// response and error
	responseEntity ResponseEntity
	httpError      *HTTPError
	internalError  error
	// other
	valueMap  map[string]interface{}
	canceled  bool
	completed bool
	crashed   bool
}

func newWebRequestContext() interface{} {
	return &WebRequestContext{
		handlerIndex: 0,
		valueMap:     make(map[string]interface{}),
	}
}

func (ctx *WebRequestContext) prepare(generateContextId bool) {
	if generateContextId {
		core.GenerateUUID(ctx.contextIdBuffer[:])
		ctx.contextIdStr = core.BytesToStr(ctx.contextIdBuffer[:])
	}
}

func (ctx *WebRequestContext) reset() {
	ctx.httpError = nil
	ctx.internalError = nil
	ctx.handlerChain = nil
	ctx.crashed = false
	ctx.canceled = false
	ctx.completed = false
	ctx.path = nil
	ctx.uri = nil
	ctx.args = nil
	ctx.handlerIndex = 0
	ctx.pathVariableCount = 0
	ctx.valueMap = nil
	ctx.responseEntity.status = http.StatusOK
	ctx.responseEntity.model = nil
	ctx.responseEntity.contentType = DefaultMediaType
}

func (ctx *WebRequestContext) writeResponse() {
	ctx.fastHttpRequestContext.SetStatusCode(ctx.responseEntity.status)
	if ctx.responseEntity.contentType == MediaTypeApplicationJson {
		ctx.fastHttpRequestContext.SetContentType(MediaTypeApplicationJsonValue)

		if ctx.responseEntity.model == nil {
			return
		}

		result, err := json.Marshal(ctx.responseEntity.model)
		if err != nil {
			panic(err)
		}
		ctx.fastHttpRequestContext.SetBody(result)
	} else if ctx.responseEntity.contentType == MediaTypeApplicationTextHtml {
		ctx.fastHttpRequestContext.SetContentType(MediaTypeApplicationTextHtmlValue)
		if ctx.responseEntity.model == nil {
			return
		}

		switch ctx.responseEntity.model.(type) {
		case string:
			value := []byte(ctx.responseEntity.model.(string))
			ctx.fastHttpRequestContext.SetBody(value)
		}
	} else {
		ctx.fastHttpRequestContext.SetContentType(MediaTypeApplicationXmlValue)

		if ctx.responseEntity.model == nil {
			return
		}

		result, err := xml.Marshal(ctx.responseEntity.model)
		if err != nil {
			panic(err)
		}
		ctx.fastHttpRequestContext.SetBody(result)
	}
}

func (ctx *WebRequestContext) invoke(recoveryActive bool, errorHandlerManager *errorHandlerManager) {
	if recoveryActive {
		defer errorHandlerManager.Recover(ctx)
		ctx.invokeHandlers(errorHandlerManager)
	} else {
		ctx.invokeHandlers(errorHandlerManager)
	}
}

func (ctx *WebRequestContext) invokeHandlers(errorHandlerManager *errorHandlerManager) {
next:
	if ctx.handlerIndex > ctx.handlerChain.handlerEndIndex {
		return
	}

	ctx.handlerChain.handlers[ctx.handlerIndex](ctx)
	if ctx.handlerIndex < ctx.handlerChain.handlerIndex && ctx.canceled {
		ctx.handlerIndex = ctx.handlerChain.afterCompletionStartIndex - 1
	}

	ctx.handlerIndex++
	if ctx.handlerIndex == ctx.handlerChain.afterCompletionStartIndex {

		if ctx.internalError == nil && ctx.httpError != nil {
			if errorHandlerManager.customErrorHandler != nil {
				errorHandlerManager.customErrorHandler.HandleError(ctx.httpError, ctx)
			} else {
				errorHandlerManager.defaultErrorHandler.HandleError(ctx.httpError, ctx)
			}
		}

		ctx.writeResponse()
		ctx.completed = true
	}

	goto next
}

func (ctx *WebRequestContext) Cancel() {
	if ctx.handlerIndex < ctx.handlerChain.handlerIndex {
		ctx.canceled = true
	}
}

func (ctx *WebRequestContext) GetContextId() context.ContextId {
	return context.ContextId(ctx.contextIdStr)
}

func (ctx *WebRequestContext) Get(key string) interface{} {
	return ctx.valueMap[key]
}

func (ctx *WebRequestContext) Put(key string, value interface{}) {
	ctx.valueMap[key] = value
}

func (ctx *WebRequestContext) addPathVariableValue(pathVariableName string) {
	ctx.pathVariables[ctx.pathVariableCount] = pathVariableName
	ctx.pathVariableCount++
}

func (ctx *WebRequestContext) getPathByteArray() []byte {
	if ctx.uri == nil {
		ctx.uri = ctx.fastHttpRequestContext.URI()
		ctx.path = ctx.uri.Path()
	}
	return ctx.path
}

func (ctx *WebRequestContext) GetPath() string {
	if len(ctx.path) == 0 {
		return string(ctx.getPathByteArray())
	}
	return string(ctx.path)
}

func (ctx *WebRequestContext) GetPathVariable(name string) (string, bool) {
	for index, pathVariableName := range ctx.handlerChain.pathVariables {
		if pathVariableName == name {
			return ctx.pathVariables[index], true
		}
	}
	return "", false
}

func (ctx *WebRequestContext) GetRequestParameter(name string) (string, bool) {
	if ctx.args == nil {
		ctx.args = ctx.fastHttpRequestContext.QueryArgs()
	}
	result := ctx.args.Peek(name)
	if result == nil {
		return "", false
	}
	return string(result), true
}

func (ctx *WebRequestContext) GetHeaderValue(key string) (string, bool) {
	val := ctx.fastHttpRequestContext.Request.Header.Peek(key)
	if val == nil {
		return "", false
	}
	return string(val), true
}

func (ctx *WebRequestContext) GetRequestBody() []byte {
	return ctx.fastHttpRequestContext.Request.Body()
}

func (ctx *WebRequestContext) BindRequest(request interface{}) {
	typ := reflect.TypeOf(request)
	if typ == nil {
		panic("Type cannot be determined as the given object is nil")
	}

	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	metadata := ctx.handlerChain.requestObjectMetadata
	if metadata == nil {
		panic("You need to specify RequestObject for handler, you cannot use BindRequest function")
	}

	if metadata.typ != typ {
		panic("Request object and type don't match.")
	}

	body := ctx.fastHttpRequestContext.Request.Body()
	if metadata.hasOnlyBody {
		contentType, ok := ctx.GetHeaderValue("Content-Type")
		if !ok {
			contentType = MediaTypeApplicationJsonValue
		}

		if contentType == MediaTypeApplicationJsonValue {
			err := json.Unmarshal(body, request)
			if err != nil {
				panic(err)
			}
		} else {
			err := xml.Unmarshal(body, request)
			if err != nil {
				panic(err)
			}
		}
		return
	}

	val := reflect.ValueOf(request)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}

	if metadata.bodyMetadata.fieldIndex != -1 {
		bodyValue := val.Field(metadata.bodyMetadata.fieldIndex)
		contentType, ok := ctx.GetHeaderValue("Content-Type")
		if !ok {
			contentType = MediaTypeApplicationJsonValue
		}

		if contentType == MediaTypeApplicationJsonValue {
			err := json.Unmarshal(body, bodyValue.Addr().Interface())
			if err != nil {
				panic(err)
			}
		} else if contentType == MediaTypeApplicationXmlValue {
			err := xml.Unmarshal(body, bodyValue.Addr().Interface())
			if err != nil {
				panic(err)
			}
		}
	}

	if metadata.paramMetadata.fieldIndex != -1 {
		paramStruct := val.Field(metadata.paramMetadata.fieldIndex)
		for tagValue, fieldMetadata := range metadata.paramMetadata.paramMap {
			paramField := paramStruct.Field(fieldMetadata.index)
			paramValue, ok := ctx.GetRequestParameter(tagValue)
			if !ok {
				continue
			}

			if fieldMetadata.converter != nil {
				paramField.Set(reflect.ValueOf(fieldMetadata.converter(paramValue)))
			} else {
				paramField.SetString(paramValue)
			}
		}
	}

	if metadata.pathMetadata.fieldIndex != -1 {
		pathStruct := val.Field(metadata.pathMetadata.fieldIndex)
		for _, fieldMetadata := range metadata.pathMetadata.pathVariableMap {
			pathField := pathStruct.Field(fieldMetadata.index)
			if fieldMetadata.extra == -1 {
				continue
			}

			pathVariableValue := ctx.pathVariables[fieldMetadata.extra]
			if fieldMetadata.converter != nil {
				pathField.Set(reflect.ValueOf(fieldMetadata.converter(pathVariableValue)))
			} else {
				pathField.SetString(pathVariableValue)
			}
		}
	}

	if metadata.headerMetadata.fieldIndex != -1 {
		headerStruct := val.Field(metadata.headerMetadata.fieldIndex)
		for tagValue, fieldMetadata := range metadata.headerMetadata.headerMap {
			headerField := headerStruct.Field(fieldMetadata.index)
			headerValue, ok := ctx.GetHeaderValue(tagValue)
			if !ok {
				continue
			}

			if fieldMetadata.converter != nil {
				headerField.Set(reflect.ValueOf(fieldMetadata.converter(headerValue)))
			} else {
				headerField.SetString(headerValue)
			}
		}
	}

}

func (ctx *WebRequestContext) SetResponseStatus(status int) ResponseBodyBuilder {
	ctx.responseEntity.status = status
	return ctx
}

func (ctx *WebRequestContext) SetModel(model interface{}) ResponseBodyBuilder {
	if model == nil {
		return ctx
	}
	ctx.responseEntity.model = model
	return ctx
}

func (ctx *WebRequestContext) GetModel() interface{} {
	return ctx.responseEntity.model
}

func (ctx *WebRequestContext) SetResponseContentType(mediaType MediaType) ResponseBodyBuilder {
	ctx.responseEntity.contentType = mediaType
	return ctx
}

func (ctx *WebRequestContext) AddHeader(key string, value string) ResponseHeaderBuilder {
	return ctx
}

func (ctx *WebRequestContext) GetResponseStatus() int {
	return ctx.responseEntity.status
}

func (ctx *WebRequestContext) GetResponseBody() []byte {
	return ctx.fastHttpRequestContext.Response.Body()
}

func (ctx *WebRequestContext) GetResponseContentType() MediaType {
	return ctx.responseEntity.contentType
}

func (ctx *WebRequestContext) Ok() ResponseBodyBuilder {
	ctx.responseEntity.status = http.StatusOK
	return ctx
}

func (ctx *WebRequestContext) NotFound() ResponseHeaderBuilder {
	ctx.responseEntity.status = http.StatusNotFound
	ctx.httpError = HttpErrorNotFound
	return ctx
}

func (ctx *WebRequestContext) NoContent() ResponseHeaderBuilder {
	ctx.responseEntity.status = http.StatusNoContent
	ctx.httpError = HttpErrorNoContent
	return ctx
}

func (ctx *WebRequestContext) BadRequest() ResponseBodyBuilder {
	ctx.responseEntity.status = http.StatusBadRequest
	ctx.httpError = HttpErrorBadRequest
	return ctx
}

func (ctx *WebRequestContext) Accepted() ResponseBodyBuilder {
	ctx.responseEntity.status = http.StatusAccepted
	ctx.httpError = nil
	return ctx
}

func (ctx *WebRequestContext) Created(location string) ResponseBodyBuilder {
	ctx.responseEntity.status = http.StatusCreated
	ctx.httpError = nil
	return ctx
}

func (ctx *WebRequestContext) GetHTTPError() *HTTPError {
	return ctx.httpError
}

func (ctx *WebRequestContext) GetInternalError() error {
	return ctx.internalError
}

func (ctx *WebRequestContext) SetHTTPError(err *HTTPError) {
	if err != nil && ctx.handlerIndex <= ctx.handlerChain.handlerIndex {
		ctx.httpError = err
	}
}

func (ctx *WebRequestContext) ThrowError(err error) {
	panic(err)
}

func (ctx *WebRequestContext) IsSuccess() bool {
	return !ctx.crashed
}

func (ctx *WebRequestContext) IsCanceled() bool {
	return ctx.completed && ctx.canceled
}

func (ctx *WebRequestContext) IsCompleted() bool {
	return ctx.completed && !ctx.canceled
}

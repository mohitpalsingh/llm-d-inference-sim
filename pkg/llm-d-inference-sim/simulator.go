/*
Copyright 2025 The llm-d-inference-sim Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package vllmsim implements the vLLM simulator.
package llmdinferencesim

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buaazp/fasthttprouter"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	vllmapi "github.com/llm-d/llm-d-inference-sim/pkg/vllm-api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"k8s.io/klog/v2"
)

const (
	vLLMDefaultPort           = 8000
	modeRandom                = "random"
	modeEcho                  = "echo"
	chatComplIDPrefix         = "chatcmpl-"
	stopFinishReason          = "stop"
	lengthFinishReason        = "length"
	toolsFinishReason         = "tool_calls"
	remoteDecodeFinishReason  = "remote_decode"
	roleAssistant             = "assistant"
	roleUser                  = "user"
	textCompletionObject      = "text_completion"
	chatCompletionObject      = "chat.completion"
	chatCompletionChunkObject = "chat.completion.chunk"
	toolChoiceNone            = "none"
	toolChoiceAuto            = "auto"
	toolChoiceRequired        = "required"
)

// VllmSimulator simulates vLLM server supporting OpenAI API
type VllmSimulator struct {
	// logger is used for information and errors logging
	logger logr.Logger
	// config is the simulator's configuration
	config *configuration
	// loraAdaptors contains list of LoRA available adaptors
	loraAdaptors sync.Map
	// runningLoras is a collection of running loras, key of lora's name, value is number of requests using this lora
	runningLoras sync.Map
	// waitingLoras will represent collection of loras defined in requests in the queue - Not implemented yet
	waitingLoras sync.Map
	// nRunningReqs is the number of inference requests that are currently being processed
	nRunningReqs int64
	// nWaitingReqs is the number of inference requests that are waiting to be processed
	nWaitingReqs int64
	// processingTokensCount tracks the total number of tokens being processed by running requests
	processingTokensCount int64
	// loraInfo is prometheus gauge
	loraInfo *prometheus.GaugeVec
	// runningRequests is prometheus gauge
	runningRequests *prometheus.GaugeVec
	// waitingRequests is prometheus gauge for number of queued requests
	waitingRequests *prometheus.GaugeVec
	// kvCacheUsagePercentage is prometheus gauge
	kvCacheUsagePercentage *prometheus.GaugeVec
	// channel for requeasts to be passed to workers
	reqChan chan *completionReqCtx
	// channel for processing queue, managed by queue manager
	processingChan chan *completionReqCtx
	// schema validator for tools parameters
	toolsValidator *validator
}

// New creates a new VllmSimulator instance with the given logger
func New(logger logr.Logger) (*VllmSimulator, error) {
	toolsValidtor, err := createValidator()
	if err != nil {
		return nil, fmt.Errorf("failed to create tools validator: %s", err)
	}
	return &VllmSimulator{
		logger:         logger,
		reqChan:        make(chan *completionReqCtx, 1000),
		processingChan: make(chan *completionReqCtx, 1000),
		toolsValidator: toolsValidtor,
	}, nil
}

// Start starts the simulator
func (s *VllmSimulator) Start(ctx context.Context) error {
	// parse command line parameters
	err := s.parseCommandParamsAndLoadConfig()
	if err != nil {
		return err
	}

	// initialize prometheus metrics
	err = s.createAndRegisterPrometheus()
	if err != nil {
		return err
	}

	// run queue manager that handles request constraints
	go s.queueManager(ctx)

	// run request processing workers
	for i := 1; i <= s.config.MaxNumSeqs; i++ {
		go s.reqProcessingWorker(ctx, i)
	}
	listener, err := s.newListener()
	if err != nil {
		return err
	}

	// start the http server
	return s.startServer(listener)
}

// parseCommandParamsAndLoadConfig parses and validates command line parameters
func (s *VllmSimulator) parseCommandParamsAndLoadConfig() error {
	config := newConfig()

	configFileValues := getParamValueFromArgs("config")
	if len(configFileValues) == 1 {
		if err := config.load(configFileValues[0]); err != nil {
			return err
		}
	}

	servedModelNames := getParamValueFromArgs("served-model-name")
	loraModuleNames := getParamValueFromArgs("lora-modules")

	f := pflag.NewFlagSet("llm-d-inference-sim flags", pflag.ContinueOnError)

	f.IntVar(&config.Port, "port", config.Port, "Port")
	f.StringVar(&config.Model, "model", config.Model, "Currently 'loaded' model")
	f.IntVar(&config.MaxNumSeqs, "max-num-seqs", config.MaxNumSeqs, "Maximum number of inference requests that could be processed at the same time (parameter to simulate requests waiting queue)")
	f.IntVar(&config.MaxNumBatchedTokens, "max-num-batched-tokens", config.MaxNumBatchedTokens, "Maximum number of batched tokens per iteration")
	f.IntVar(&config.MaxLoras, "max-loras", config.MaxLoras, "Maximum number of LoRAs in a single batch")
	f.IntVar(&config.MaxCPULoras, "max-cpu-loras", config.MaxCPULoras, "Maximum number of LoRAs to store in CPU memory")
	f.IntVar(&config.MaxModelLen, "max-model-len", config.MaxModelLen, "Model's context window, maximum number of tokens in a single request including input and output")

	f.StringVar(&config.Mode, "mode", config.Mode, "Simulator mode, echo - returns the same text that was sent in the request, for chat completion returns the last message, random - returns random sentence from a bank of pre-defined sentences")
	f.IntVar(&config.InterTokenLatency, "inter-token-latency", config.InterTokenLatency, "Time to generate one token (in milliseconds)")
	f.IntVar(&config.TimeToFirstToken, "time-to-first-token", config.TimeToFirstToken, "Time to first token (in milliseconds)")
	f.IntVar(&config.KVCacheTransferLatency, "kv-cache-transfer-latency", config.KVCacheTransferLatency, "Time for KV-cache transfer from a remote vLLM (in milliseconds)")
	f.Int64Var(&config.Seed, "seed", config.Seed, "Random seed for operations (if not set, current Unix time in nanoseconds is used)")

	// These values were manually parsed above in getParamValueFromArgs, we leave this in order to get these flags in --help
	var dummyString string
	f.StringVar(&dummyString, "config", "", "The path to a yaml configuration file. The command line values overwrite the configuration file values")
	var dummyMultiString multiString
	f.Var(&dummyMultiString, "served-model-name", "Model names exposed by the API (a list of space-separated strings)")
	f.Var(&dummyMultiString, "lora-modules", "List of LoRA adapters (a list of space-separated JSON strings)")
	// In order to allow empty arguments, we set a dummy NoOptDefVal for these flags
	f.Lookup("served-model-name").NoOptDefVal = "dummy"
	f.Lookup("lora-modules").NoOptDefVal = "dummy"

	flagSet := flag.NewFlagSet("simFlagSet", flag.ExitOnError)
	klog.InitFlags(flagSet)
	f.AddGoFlagSet(flagSet)

	if err := f.Parse(os.Args[1:]); err != nil {
		if err == pflag.ErrHelp {
			// --help - exit without printing an error message
			os.Exit(0)
		}
		return err
	}

	// Need to read in a variable to avoid merging the values with the config file ones
	if loraModuleNames != nil {
		config.LoraModulesString = loraModuleNames
		if err := config.unmarshalLoras(); err != nil {
			return err
		}
	}
	if servedModelNames != nil {
		config.ServedModelNames = servedModelNames
	}

	if err := config.validate(); err != nil {
		return err
	}

	s.config = config

	for _, lora := range config.LoraModules {
		s.loraAdaptors.Store(lora.Name, "")
	}

	initRandom(s.config.Seed)

	// just to suppress not used lint error for now
	_ = &s.waitingLoras
	return nil
}

func getParamValueFromArgs(param string) []string {
	var values []string
	var readValues bool
	for _, arg := range os.Args[1:] {
		if readValues {
			if strings.HasPrefix(arg, "--") {
				break
			}
			if arg != "" {
				values = append(values, arg)
			}
		} else {
			if arg == "--"+param {
				readValues = true
				values = make([]string, 0)
			} else if strings.HasPrefix(arg, "--"+param+"=") {
				// Handle --param=value
				values = append(values, strings.TrimPrefix(arg, "--"+param+"="))
				break
			}
		}
	}
	return values
}

func (s *VllmSimulator) newListener() (net.Listener, error) {
	s.logger.Info("Server starting", "port", s.config.Port)
	listener, err := net.Listen("tcp4", fmt.Sprintf(":%d", s.config.Port))
	if err != nil {
		return nil, err
	}
	return listener, nil
}

// startServer starts http server on port defined in command line
func (s *VllmSimulator) startServer(listener net.Listener) error {
	r := fasthttprouter.New()

	// support completion APIs
	r.POST("/v1/chat/completions", s.HandleChatCompletions)
	r.POST("/v1/completions", s.HandleTextCompletions)
	// supports /models API
	r.GET("/v1/models", s.HandleModels)
	// support load/unload of lora adapter
	r.POST("/v1/load_lora_adapter", s.HandleLoadLora)
	r.POST("/v1/unload_lora_adapter", s.HandleUnloadLora)
	// supports /metrics prometheus API
	r.GET("/metrics", fasthttpadaptor.NewFastHTTPHandler(promhttp.Handler()))
	// supports standard Kubernetes health and readiness checks
	r.GET("/health", s.HandleHealth)
	r.GET("/ready", s.HandleReady)

	server := fasthttp.Server{
		ErrorHandler: s.HandleError,
		Handler:      r.Handler,
		Logger:       s,
	}

	defer func() {
		if err := listener.Close(); err != nil {
			s.logger.Error(err, "server listener close failed")
		}
	}()

	return server.Serve(listener)
}

// Print prints to a log, implementation of fasthttp.Logger
func (s *VllmSimulator) Printf(format string, args ...interface{}) {
	s.logger.Info("Server error", "msg", fmt.Sprintf(format, args...))
}

// readRequest reads and parses data from the body of the given request according the type defined by isChatCompletion
func (s *VllmSimulator) readRequest(ctx *fasthttp.RequestCtx, isChatCompletion bool) (completionRequest, error) {
	if isChatCompletion {
		var req chatCompletionRequest

		err := json.Unmarshal(ctx.Request.Body(), &req)
		if err != nil {
			s.logger.Error(err, "failed to unmarshal request body")
			return nil, err
		}

		for _, tool := range req.Tools {
			toolJson, err := json.Marshal(tool.Function)
			if err != nil {
				s.logger.Error(err, "failed to marshal request tools")
				return nil, err
			}
			err = s.toolsValidator.validateTool(toolJson)
			if err != nil {
				s.logger.Error(err, "tool validation failed")
				return nil, err
			}
		}

		return &req, nil
	}

	var req textCompletionRequest
	err := json.Unmarshal(ctx.Request.Body(), &req)

	return &req, err
}

// HandleChatCompletions http handler for /v1/chat/completions
func (s *VllmSimulator) HandleChatCompletions(ctx *fasthttp.RequestCtx) {
	s.logger.Info("chat completion request received")
	s.handleCompletions(ctx, true)
}

// HandleTextCompletions http handler for /v1/completions
func (s *VllmSimulator) HandleTextCompletions(ctx *fasthttp.RequestCtx) {
	s.logger.Info("completion request received")
	s.handleCompletions(ctx, false)
}

func (s *VllmSimulator) HandleLoadLora(ctx *fasthttp.RequestCtx) {
	s.logger.Info("load lora request received")
	s.loadLora(ctx)
}

func (s *VllmSimulator) HandleUnloadLora(ctx *fasthttp.RequestCtx) {
	s.logger.Info("unload lora request received")
	s.unloadLora(ctx)
}

func (s *VllmSimulator) validateRequest(req completionRequest) (string, string, int) {
	if !s.isValidModel(req.getModel()) {
		return fmt.Sprintf("The model `%s` does not exist.", req.getModel()), "NotFoundError", fasthttp.StatusNotFound
	}

	if req.doRemoteDecode() && req.isStream() {
		return "Prefill does not support streaming", "Invalid request", fasthttp.StatusBadRequest
	}

	return "", "", fasthttp.StatusOK
}

// isValidModel checks if the given model is the base model or one of "loaded" LoRAs
func (s *VllmSimulator) isValidModel(model string) bool {
	for _, name := range s.config.ServedModelNames {
		if model == name {
			return true
		}
	}
	for _, lora := range s.getLoras() {
		if model == lora {
			return true
		}
	}

	return false
}

// isLora returns true if the given model name is one of loaded LoRAs
func (s *VllmSimulator) isLora(model string) bool {
	for _, lora := range s.getLoras() {
		if model == lora {
			return true
		}
	}

	return false
}

// calculateProcessingTokens calculates the total number of processing tokens for a request
// Returns prompt tokens + max output tokens, or MaxModelLen if max_tokens is not specified
func (s *VllmSimulator) calculateProcessingTokens(req completionRequest) int {
	promptTokens := req.getNumberOfPromptTokens()
	maxCompletionTokens := req.getMaxCompletionTokens()

	// If max_tokens is not specified, return the maximum possible tokens (MaxModelLen)
	if maxCompletionTokens == nil {
		return s.config.MaxModelLen
	}

	// If max_tokens is specified, return prompt tokens + specified max completion tokens
	return promptTokens + int(*maxCompletionTokens)
}

// canAcceptRequest checks if a new request can be accepted based on max-num-seqs and max-num-batched-tokens constraints
func (s *VllmSimulator) canAcceptRequest(req completionRequest) bool {
	currentRunning := atomic.LoadInt64(&s.nRunningReqs)

	// Check max-num-seqs constraint
	if currentRunning >= int64(s.config.MaxNumSeqs) {
		return false
	}

	// If max-num-batched-tokens is not configured (0), only check max-num-seqs
	if s.config.MaxNumBatchedTokens <= 0 {
		return true
	}

	// Calculate tokens needed for this request
	requestTokens := s.calculateProcessingTokens(req)
	currentTokens := atomic.LoadInt64(&s.processingTokensCount)

	// Check max-num-batched-tokens constraint
	return currentTokens+int64(requestTokens) <= int64(s.config.MaxNumBatchedTokens)
}

// addRunningRequest adds a request to the running requests tracking
func (s *VllmSimulator) addRunningRequest(reqCtx *completionReqCtx) {
	processingTokens := s.calculateProcessingTokens(reqCtx.completionReq)
	reqCtx.processingTokens = processingTokens

	atomic.AddInt64(&s.processingTokensCount, int64(processingTokens))
	atomic.AddInt64(&s.nRunningReqs, 1)
}

// removeRunningRequest removes a request from the running requests tracking
func (s *VllmSimulator) removeRunningRequest(reqCtx *completionReqCtx) {
	atomic.AddInt64(&s.processingTokensCount, -int64(reqCtx.processingTokens))
	atomic.AddInt64(&s.nRunningReqs, -1)
}

// handleCompletions general completion requests handler, support both text and chat completion APIs
func (s *VllmSimulator) handleCompletions(ctx *fasthttp.RequestCtx, isChatCompletion bool) {
	vllmReq, err := s.readRequest(ctx, isChatCompletion)
	if err != nil {
		s.logger.Error(err, "failed to read and parse request body")
		ctx.Error("Failed to read and parse request body, "+err.Error(), fasthttp.StatusBadRequest)
		return
	}

	errMsg, errType, errCode := s.validateRequest(vllmReq)
	if errMsg != "" {
		s.sendCompletionError(ctx, errMsg, errType, errCode)
		return
	}

	// Validate context window constraints
	promptTokens := vllmReq.getNumberOfPromptTokens()
	completionTokens := vllmReq.getMaxCompletionTokens()
	isValid, actualCompletionTokens, totalTokens := validateContextWindow(promptTokens, completionTokens, s.config.MaxModelLen)
	if !isValid {
		s.sendCompletionError(ctx, fmt.Sprintf("This model's maximum context length is %d tokens. However, you requested %d tokens (%d in the messages, %d in the completion). Please reduce the length of the messages or completion",
			s.config.MaxModelLen, totalTokens, promptTokens, actualCompletionTokens), "BadRequestError", fasthttp.StatusBadRequest)
		return
	}

	// Validate max-num-batched-tokens constraint - reject requests that would never be accepted
	if s.config.MaxNumBatchedTokens > 0 {
		requestTokens := s.calculateProcessingTokens(vllmReq)
		if requestTokens > s.config.MaxNumBatchedTokens {
			s.sendCompletionError(ctx, fmt.Sprintf("Request requires %d tokens, but max-num-batched-tokens is set to %d. This request would never be accepted. Please reduce max_tokens or increase max-num-batched-tokens",
				requestTokens, s.config.MaxNumBatchedTokens), "BadRequestError", fasthttp.StatusBadRequest)
			return
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	reqCtx := &completionReqCtx{
		completionReq:    vllmReq,
		httpReqCtx:       ctx,
		isChatCompletion: isChatCompletion,
		wg:               &wg,
	}
	s.reqChan <- reqCtx
	atomic.StoreInt64(&(s.nWaitingReqs), int64(len(s.reqChan)))
	s.reportWaitingRequests()
	wg.Wait()
}

func (s *VllmSimulator) queueManager(ctx context.Context) {
	// Use a slice to maintain the queue of waiting requests
	var waitingQueue []*completionReqCtx
	ticker := time.NewTicker(10 * time.Millisecond) // Check every 10ms if we can process waiting requests
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("queueManager stopped")
			return
		case reqCtx := <-s.reqChan:
			// Add new request to the waiting queue
			waitingQueue = append(waitingQueue, reqCtx)
		case <-ticker.C:
			// Periodically check if we can process waiting requests
			if len(waitingQueue) == 0 {
				continue
			}

			// Try to process requests from the front of the queue
			var newQueue []*completionReqCtx
			for _, reqCtx := range waitingQueue {
				if s.canAcceptRequest(reqCtx.completionReq) {
					// Add to running requests tracking
					s.addRunningRequest(reqCtx)

					// Send to processing channel
					s.processingChan <- reqCtx
				} else {
					// Can't process yet, keep in queue
					newQueue = append(newQueue, reqCtx)
				}
			}
			waitingQueue = newQueue
		}
	}
}

func (s *VllmSimulator) reqProcessingWorker(ctx context.Context, id int) {
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("reqProcessingWorker stopped:", "worker id", id)
			return
		case reqCtx, ok := <-s.processingChan:
			if !ok {
				s.logger.Info("reqProcessingWorker worker exiting: processingChan closed")
				return
			}
			atomic.StoreInt64(&(s.nWaitingReqs), int64(len(s.reqChan)))
			s.reportWaitingRequests()

			req := reqCtx.completionReq
			model := req.getModel()
			displayModel := s.getDisplayedModelName(model)

			if s.isLora(model) {
				// if current request's model is LoRA, add it to the list of running loras
				value, ok := s.runningLoras.Load(model)
				intValue := 0

				if !ok {
					s.logger.Info("Create reference counter", "model", model)
					intValue = 0
				} else {
					intValue = value.(int)
				}
				s.runningLoras.Store(model, intValue+1)
				s.logger.Info("Update LoRA reference counter", "model", model, "old value", intValue, "new value", intValue+1)

				// TODO - check if this request went to the waiting queue - add it to waiting map
				s.reportLoras()
			}

			// Note: we don't increment nRunningReqs here because it's already done in addRunningRequest
			s.reportRunningRequests()

			var responseTokens []string
			var finishReason string
			var err error
			var toolCalls []toolCall
			var completionTokens int
			if reqCtx.isChatCompletion &&
				req.getToolChoice() != toolChoiceNone &&
				req.getTools() != nil {
				toolCalls, finishReason, completionTokens, err =
					createToolCalls(req.getTools(), req.getToolChoice())
			}
			if toolCalls == nil && err == nil {
				// Either no tool calls were defined, or we randomly chose not to create tool calls,
				// so we generate a response text.
				responseTokens, finishReason, completionTokens, err = req.createResponseText(s.config.Mode)
			}
			if err != nil {
				prefix := ""
				if reqCtx.isChatCompletion {
					prefix = "failed to create chat response"
				} else {
					prefix = "failed to create text response"
				}
				s.logger.Error(err, prefix)
				reqCtx.httpReqCtx.Error(prefix+err.Error(), fasthttp.StatusBadRequest)
			} else {
				usageData := usage{
					PromptTokens:     req.getNumberOfPromptTokens(),
					CompletionTokens: completionTokens,
					TotalTokens:      req.getNumberOfPromptTokens() + completionTokens,
				}
				if req.isStream() {
					var usageDataToSend *usage
					if req.includeUsage() {
						usageDataToSend = &usageData
					}
					s.sendStreamingResponse(
						&streamingContext{
							ctx:              reqCtx.httpReqCtx,
							isChatCompletion: reqCtx.isChatCompletion,
							model:            displayModel,
							doRemotePrefill:  req.doRemotePrefill(),
						},
						responseTokens, toolCalls, finishReason, usageDataToSend,
					)
				} else {
					if req.doRemoteDecode() {
						// in case this is prefill pod processing, return special finish reason
						finishReason = remoteDecodeFinishReason
					}

					s.sendResponse(reqCtx.isChatCompletion,
						reqCtx.httpReqCtx,
						responseTokens,
						toolCalls,
						displayModel,
						finishReason,
						&usageData,
						req.doRemoteDecode(),
						req.doRemotePrefill())
				}
			}

			// Clean up the running request tracking
			s.removeRunningRequest(reqCtx)

			reqCtx.wg.Done()
		}
	}
}

// decrease model usage reference number
func (s *VllmSimulator) responseSentCallback(model string) {
	// Note: nRunningReqs is now decremented in removeRunningRequest
	s.reportRunningRequests()

	// Only LoRA models require reference-count handling.
	if !s.isLora(model) {
		return
	}

	value, ok := s.runningLoras.Load(model)

	if !ok {
		s.logger.Info("Error: nil reference counter", "model", model)
		s.logger.Error(nil, "Zero model reference", "model", model)
	} else {
		intValue := value.(int)
		if intValue > 1 {
			s.runningLoras.Store(model, intValue-1)
			s.logger.Info("Update LoRA reference counter", "model", model, "prev value", intValue, "new value", intValue-1)
		} else {
			// last lora instance stopped its execution - remove from the map
			s.runningLoras.Delete(model)
			s.logger.Info("Remove LoRA from set of running loras", "model", model)
		}
	}

	s.reportLoras()
}

// sendCompletionError sends an error response for the current completion request
func (s *VllmSimulator) sendCompletionError(ctx *fasthttp.RequestCtx, msg string, errType string, code int) {
	compErr := completionError{
		Object:  "error",
		Message: msg,
		Type:    errType,
		Code:    code,
		Param:   nil,
	}
	s.logger.Error(nil, compErr.Message)

	data, err := json.Marshal(compErr)
	if err != nil {
		ctx.Error(err.Error(), fasthttp.StatusInternalServerError)
	} else {
		ctx.SetContentType("application/json")
		ctx.SetStatusCode(code)
		ctx.SetBody(data)
	}
}

// HandleModels handles /v1/models request according the data stored in the simulator
func (s *VllmSimulator) HandleModels(ctx *fasthttp.RequestCtx) {
	modelsResp := s.createModelsResponse()

	data, err := json.Marshal(modelsResp)
	if err != nil {
		s.logger.Error(err, "Failed to marshal models response")
		ctx.Error("Failed to marshal models response, "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	ctx.Response.Header.SetContentType("application/json")
	ctx.Response.Header.SetStatusCode(fasthttp.StatusOK)
	ctx.Response.SetBody(data)
}

func (s *VllmSimulator) HandleError(_ *fasthttp.RequestCtx, err error) {
	s.logger.Error(err, "VLLM server error")
}

// createCompletionResponse creates the response for completion requests, supports both completion request types (text and chat)
// as defined by isChatCompletion
// respTokens - tokenized content to be sent in the response
// toolCalls - tool calls to be sent in the response
// finishReason - a pointer to string that represents finish reason, can be nil or stop or length, ...
// usageData - usage (tokens statistics) for this response
// modelName - display name returned to the client and used in metrics. It is either the first alias
// from --served-model-name (for a base-model request) or the LoRA adapter name (for a LoRA request).
func (s *VllmSimulator) createCompletionResponse(isChatCompletion bool, respTokens []string, toolCalls []toolCall,
	finishReason *string, usageData *usage, modelName string, doRemoteDecode bool) completionResponse {
	baseResp := baseCompletionResponse{
		ID:      chatComplIDPrefix + uuid.NewString(),
		Created: time.Now().Unix(),
		Model:   modelName,
		Usage:   usageData,
	}

	if doRemoteDecode {
		// add special fields related to the prefill pod special behavior
		baseResp.DoRemoteDecode = true
		baseResp.DoRemotePrefill = false
		// currently remote prefill information is hard-coded
		baseResp.RemoteBlockIds = []string{"DUMMY_ID"}
		baseResp.RemoteEngineId = "DUMMY_ID"
		baseResp.RemoteHost = "DUMMY"
		baseResp.RemotePort = 1234
	}

	baseChoice := baseResponseChoice{Index: 0, FinishReason: finishReason}

	respText := strings.Join(respTokens, "")
	if isChatCompletion {
		baseResp.Object = chatCompletionObject

		message := message{Role: roleAssistant}
		if toolCalls != nil {
			message.ToolCalls = toolCalls
		} else {
			message.Content = content{Raw: respText}
		}
		return &chatCompletionResponse{
			baseCompletionResponse: baseResp,
			Choices:                []chatRespChoice{{Message: message, baseResponseChoice: baseChoice}},
		}
	}

	baseResp.Object = textCompletionObject
	return &textCompletionResponse{
		baseCompletionResponse: baseResp,
		Choices:                []textRespChoice{{baseResponseChoice: baseChoice, Text: respText}},
	}
}

// sendResponse sends response for completion API, supports both completions (text and chat)
// according the value of isChatCompletion
// respTokens - tokenized content to be sent in the response
// toolCalls - tool calls to be sent in the response
// modelName - display name returned to the client and used in metrics. It is either the first alias
// from --served-model-name (for a base-model request) or the LoRA adapter name (for a LoRA request).
// finishReason - a pointer to string that represents finish reason, can be nil, stop, length, or tools
// usageData - usage (tokens statistics) for this response
func (s *VllmSimulator) sendResponse(isChatCompletion bool, ctx *fasthttp.RequestCtx, respTokens []string, toolCalls []toolCall,
	modelName string, finishReason string, usageData *usage, doRemoteDecode bool, doRemotePrefill bool) {
	resp := s.createCompletionResponse(isChatCompletion, respTokens, toolCalls, &finishReason, usageData, modelName, doRemoteDecode)

	data, err := json.Marshal(resp)
	if err != nil {
		ctx.Error("Response body creation failed, "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	// calculate how long to wait before returning the response, time is based on number of tokens
	numOfTokens := usageData.CompletionTokens
	totalMillisToWait := s.getTimeToFirstToken(doRemotePrefill) + (numOfTokens-1)*s.config.InterTokenLatency
	time.Sleep(time.Duration(totalMillisToWait) * time.Millisecond)

	// TODO - maybe add pod id to response header for testing
	ctx.Response.Header.SetContentType("application/json")
	ctx.Response.Header.SetStatusCode(fasthttp.StatusOK)
	ctx.Response.SetBody(data)

	s.responseSentCallback(modelName)
}

// returns time to first token based on the current request's doRemotePrefill
func (s *VllmSimulator) getTimeToFirstToken(doRemotePrefill bool) int {
	if doRemotePrefill {
		return s.config.KVCacheTransferLatency
	}
	return s.config.TimeToFirstToken
}

// createModelsResponse creates and returns ModelResponse for the current state, returned array of models contains the base model + LoRA adapters if exist
func (s *VllmSimulator) createModelsResponse() *vllmapi.ModelsResponse {
	modelsResp := vllmapi.ModelsResponse{Object: "list", Data: []vllmapi.ModelsResponseModelInfo{}}

	// Advertise every public model alias
	for _, alias := range s.config.ServedModelNames {
		modelsResp.Data = append(modelsResp.Data, vllmapi.ModelsResponseModelInfo{
			ID:      alias,
			Object:  vllmapi.ObjectModel,
			Created: time.Now().Unix(),
			OwnedBy: "vllm",
			Root:    alias,
			Parent:  nil,
		})
	}

	// add LoRA adapter's info
	parent := s.config.ServedModelNames[0]
	for _, lora := range s.getLoras() {
		modelsResp.Data = append(modelsResp.Data, vllmapi.ModelsResponseModelInfo{
			ID:      lora,
			Object:  vllmapi.ObjectModel,
			Created: time.Now().Unix(),
			OwnedBy: "vllm",
			Root:    lora,
			Parent:  &parent,
		})
	}

	return &modelsResp
}

// HandleHealth http handler for /health
func (s *VllmSimulator) HandleHealth(ctx *fasthttp.RequestCtx) {
	s.logger.V(4).Info("health request received")
	ctx.Response.Header.SetContentType("application/json")
	ctx.Response.Header.SetStatusCode(fasthttp.StatusOK)
	ctx.Response.SetBody([]byte("{}"))
}

// HandleReady http handler for /ready
func (s *VllmSimulator) HandleReady(ctx *fasthttp.RequestCtx) {
	s.logger.V(4).Info("readiness request received")
	ctx.Response.Header.SetContentType("application/json")
	ctx.Response.Header.SetStatusCode(fasthttp.StatusOK)
	ctx.Response.SetBody([]byte("{}"))
}

// getDisplayedModelName returns the model name that must appear in API
// responses.  LoRA adapters keep their explicit name, while all base-model
// requests are surfaced as the first alias from --served-model-name.
func (s *VllmSimulator) getDisplayedModelName(reqModel string) string {
	if s.isLora(reqModel) {
		return reqModel
	}
	return s.config.ServedModelNames[0]
}

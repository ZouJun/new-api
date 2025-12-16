// Package controller 包含所有 HTTP 控制器处理函数
// 负责接收请求、调用服务层、返回响应
package controller

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// relayHandler 根据不同的代理模式路由到对应的处理函数
// 参数:
//
//	c: Gin 上下文对象
//	info: 代理请求的元信息
//
// 返回值:
//
//	*types.NewAPIError: 如果发生错误则返回错误对象
//
// 支持的模式:
//   - 图像生成/编辑
//   - 音频语音合成/翻译/转写
//   - 重排序 (Rerank)
//   - 嵌入向量 (Embeddings)
//   - 统一响应处理 (Responses)
//   - 默认文本对话模式
func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		err = relay.ImageHelper(c, info)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, info)
	case relayconstant.RelayModeRerank:
		err = relay.RerankHelper(c, info)
	case relayconstant.RelayModeEmbeddings:
		err = relay.EmbeddingHelper(c, info)
	case relayconstant.RelayModeResponses:
		err = relay.ResponsesHelper(c, info)
	default:
		err = relay.TextHelper(c, info)
	}
	return err
}

// geminiRelayHandler 专门处理 Gemini 模型的代理请求
// 参数:
//
//	c: Gin 上下文对象
//	info: 代理请求的元信息
//
// 返回值:
//
//	*types.NewAPIError: 如果发生错误则返回错误对象
//
// 说明:
//
//	根据 URL 路径判断是嵌入向量请求还是普通对话请求
func geminiRelayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	if strings.Contains(c.Request.URL.Path, "embed") {
		err = relay.GeminiEmbeddingHandler(c, info)
	} else {
		err = relay.GeminiHelper(c, info)
	}
	return err
}

// Relay 是核心的 API 代理处理函数
// 参数:
//
//	c: Gin 上下文对象
//	relayFormat: 代理格式，如 OpenAI、Claude、Gemini 等
//
// 说明:
//
//	主要流程:
//	1. 判断是否为 WebSocket 实时连接，如是则升级协议
//	2. 验证请求参数和格式
//	3. 生成代理元信息并计算预估 token 数
//	4. 计算价格并预扣费（非免费模型）
//	5. 选择可用渠道并发起请求，失败时重试其他渠道
//	6. 处理响应并根据实际使用量结算费用
func Relay(c *gin.Context, relayFormat types.RelayFormat) {

	// 获取请求 ID，用于日志跟踪
	requestId := c.GetString(common.RequestIdKey)
	//group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	//originalModel := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)

	var (
		newAPIError *types.NewAPIError // 用于存储错误信息
		ws          *websocket.Conn    // WebSocket 连接对象
	)

	// 如果是实时对话 API，升级 HTTP 连接为 WebSocket
	if relayFormat == types.RelayFormatOpenAIRealtime {
		var err error
		// 升级协议
		ws, err = upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			helper.WssError(c, ws, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry()).ToOpenAIError())
			return
		}
		defer ws.Close()
	}

	// defer 函数用于统一处理错误响应，根据不同的 API 格式返回适当的错误格式
	defer func() {
		if newAPIError != nil {
			logger.LogError(c, fmt.Sprintf("relay error: %s", newAPIError.Error()))
			// 在错误信息中添加请求 ID，方便追踪
			newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))
			switch relayFormat {
			case types.RelayFormatOpenAIRealtime:
				// WebSocket 实时连接错误
				helper.WssError(c, ws, newAPIError.ToOpenAIError())
			case types.RelayFormatClaude:
				// Claude 格式错误
				c.JSON(newAPIError.StatusCode, gin.H{
					"type":  "error",
					"error": newAPIError.ToClaudeError(),
				})
			default:
				// OpenAI 默认格式错误
				c.JSON(newAPIError.StatusCode, gin.H{
					"error": newAPIError.ToOpenAIError(),
				})
			}
		}
	}()

	// 获取并验证请求参数
	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest)
		return
	}

	// 生成代理请求的元信息（模型、渠道、配置等）
	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeGenRelayInfoFailed)
		return
	}

	// 获取用于 token 计数的元数据
	meta := request.GetTokenCountMeta()

	// 如果启用了敏感词检测，检查请求中的文本内容
	if setting.ShouldCheckPromptSensitive() {
		contains, words := service.CheckSensitiveText(meta.CombineText)
		if contains {
			logger.LogWarn(c, fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", ")))
			newAPIError = types.NewError(err, types.ErrorCodeSensitiveWordsDetected)
			return
		}
	}

	// 估算请求会消耗的 token 数量
	tokens, err := service.EstimateRequestToken(c, meta, relayInfo)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeCountTokenFailed)
		return
	}

	// 设置预估的 prompt token 数
	relayInfo.SetEstimatePromptTokens(tokens)

	// 计算模型价格和需要预扣的配额
	priceData, err := helper.ModelPriceHelper(c, relayInfo, tokens, meta)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError)
		return
	}

	// common.SetContextKey(c, constant.ContextKeyTokenCountMeta, meta)

	// 如果是免费模型，跳过预扣费；否则预先扣除配额
	if priceData.FreeModel {
		logger.LogInfo(c, fmt.Sprintf("模型 %s 免费，跳过预扣费", relayInfo.OriginModelName))
	} else {
		newAPIError = service.PreConsumeQuota(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	// defer 函数：如果下游请求失败且实际预扣了配额，则退还配额
	defer func() {
		// Only return quota if downstream failed and quota was actually pre-consumed
		if newAPIError != nil && relayInfo.FinalPreConsumedQuota != 0 {
			service.ReturnPreConsumedQuota(c, relayInfo)
		}
	}()

	// 初始化重试参数
	retryParam := &service.RetryParam{
		Ctx:        c,
		TokenGroup: relayInfo.TokenGroup,
		ModelName:  relayInfo.OriginModelName,
		Retry:      common.GetPointer(0),
	}

	// 重试循环：尝试不同的渠道，直到成功或超过最大重试次数
	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		// 获取可用渠道
		channel, err := getChannel(c, relayInfo, retryParam)
		if err != nil {
			logger.LogError(c, err.Error())
			newAPIError = err
			break
		}

		// 记录已使用的渠道 ID
		addUsedChannel(c, channel.Id)
		// 重置请求 Body，因为重试时需要重新读取
		requestBody, _ := common.GetRequestBody(c)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))

		// 根据不同的 API 格式调用对应的处理函数
		switch relayFormat {
		case types.RelayFormatOpenAIRealtime:
			// WebSocket 实时对话
			newAPIError = relay.WssHelper(c, relayInfo)
		case types.RelayFormatClaude:
			// Claude API 格式
			newAPIError = relay.ClaudeHelper(c, relayInfo)
		case types.RelayFormatGemini:
			// Gemini API 格式
			newAPIError = geminiRelayHandler(c, relayInfo)
		default:
			// 其他格式（OpenAI 等）
			newAPIError = relayHandler(c, relayInfo)
		}

		// 如果成功，直接返回
		if newAPIError == nil {
			return
		}

		// 处理渠道错误（记录日志、禁用渠道等）
		processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)

		// 判断是否应该重试
		if !shouldRetry(c, newAPIError, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	// 获取所有已尝试的渠道 ID 列表
	useChannel := c.GetStringSlice("use_channel")
	// 如果进行了重试，记录重试路径
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
}

// upgrader WebSocket 协议升级器配置
var upgrader = websocket.Upgrader{
	Subprotocols: []string{"realtime"}, // WS 握手支持的协议，如果有使用 Sec-WebSocket-Protocol，则必须在此声明对应的 Protocol TODO add other protocol
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域
	},
}

// addUsedChannel 将已使用的渠道 ID 添加到上下文中
// 参数:
//
//	c: Gin 上下文对象
//	channelId: 渠道 ID
//
// 说明:
//
//	用于记录重试过程中使用过的所有渠道，方便排查问题
func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

// getChannel 获取可用的代理渠道
// 参数:
//
//	c: Gin 上下文对象
//	info: 代理请求的元信息
//	retryParam: 重试参数
//
// 返回值:
//
//	*model.Channel: 选中的渠道对象
//	*types.NewAPIError: 如果获取失败则返回错误
//
// 说明:
//  1. 如果已有指定渠道，直接返回
//  2. 否则从缓存中随机选择一个满足条件的渠道
func getChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	// 如果已经有指定渠道（如通过特定 API 路径），直接使用
	if info.ChannelMeta == nil {
		autoBan := c.GetBool("auto_ban")
		autoBanInt := 1
		if !autoBan {
			autoBanInt = 0
		}
		return &model.Channel{
			Id:      c.GetInt("channel_id"),
			Type:    c.GetInt("channel_type"),
			Name:    c.GetString("channel_name"),
			AutoBan: &autoBanInt,
		}, nil
	}
	// 从缓存中随机获取一个满足条件的渠道
	channel, selectGroup, err := service.CacheGetRandomSatisfiedChannel(retryParam)

	// 处理分组比率，用于计费
	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	if err != nil {
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的可用渠道失败（retry）: %s", selectGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的可用渠道不存在（retry）", selectGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	// 为选中的渠道设置上下文信息
	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName)
	if newAPIError != nil {
		return nil, newAPIError
	}
	return channel, nil
}

// shouldRetry 判断是否应该重试请求
// 参数:
//
//	c: Gin 上下文对象
//	openaiErr: 错误对象
//	retryTimes: 剩余重试次数
//
// 返回值:
//
//	bool: true 表示应该重试，false 表示不应重试
//
// 重试策略:
//   - 渠道错误: 重试
//   - 明确标记为跳过重试的错误: 不重试
//   - 没有剩余重试次数: 不重试
//   - 指定了特定渠道: 不重试
//   - 429 限流错误: 重试
//   - 307 重定向: 重试
//   - 5xx 服务器错误: 重试（但 504/524 超时除外）
//   - 400/408 客户端错误: 不重试
func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	// 如果没有错误，不重试
	if openaiErr == nil {
		return false
	}
	// 如果是渠道错误，应该重试
	if types.IsChannelError(openaiErr) {
		return true
	}
	// 如果明确标记为跳过重试，不重试
	if types.IsSkipRetryError(openaiErr) {
		return false
	}
	// 如果没有剩余重试次数，不重试
	if retryTimes <= 0 {
		return false
	}
	// 如果指定了特定渠道，不重试
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	// 429 限流错误，应该重试
	if openaiErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	// 307 重定向，应该重试
	if openaiErr.StatusCode == 307 {
		return true
	}
	// 5xx 服务器错误，应该重试
	if openaiErr.StatusCode/100 == 5 {
		// 但是超时错误（504, 524）不重试
		if openaiErr.StatusCode == 504 || openaiErr.StatusCode == 524 {
			return false
		}
		return true
	}
	// 400 客户端错误，不重试
	if openaiErr.StatusCode == http.StatusBadRequest {
		return false
	}
	// 408 Azure 处理超时，不重试
	if openaiErr.StatusCode == 408 {
		return false
	}
	// 2xx 成功状态码，不重试
	if openaiErr.StatusCode/100 == 2 {
		return false
	}
	// 其他情况默认重试
	return true
}

// processChannelError 处理渠道错误
// 参数:
//
//	c: Gin 上下文对象
//	channelError: 渠道错误信息
//	err: 错误对象
//
// 说明:
//  1. 记录错误日志
//  2. 如果需要，自动禁用失败的渠道
//  3. 如果启用了错误日志，保存错误记录到数据库
func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError) {
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", channelError.ChannelId, err.StatusCode, err.Error()))
	// 不要使用 context 获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	// 如果应该禁用渠道且允许自动封禁，异步禁用渠道
	if service.ShouldDisableChannel(channelError.ChannelType, err) && channelError.AutoBan {
		gopool.Go(func() {
			service.DisableChannel(channelError, err.Error())
		})
	}

	// 如果启用了错误日志且需要记录此错误，保存到数据库
	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		// 保存错误日志到 mysql 中
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		channelId := c.GetInt("channel_id")
		// 构建附加信息
		other := make(map[string]interface{})
		if c.Request != nil && c.Request.URL != nil {
			other["request_path"] = c.Request.URL.Path
		}
		other["error_type"] = err.GetErrorType()
		other["error_code"] = err.GetErrorCode()
		other["status_code"] = err.StatusCode
		other["channel_id"] = channelId
		other["channel_name"] = c.GetString("channel_name")
		other["channel_type"] = c.GetInt("channel_type")
		// 构建管理员信息
		adminInfo := make(map[string]interface{})
		adminInfo["use_channel"] = c.GetStringSlice("use_channel")
		// 如果是多密钥渠道，记录相关信息
		isMultiKey := common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey)
		if isMultiKey {
			adminInfo["is_multi_key"] = true
			adminInfo["multi_key_index"] = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
		}
		other["admin_info"] = adminInfo
		// 记录错误日志到数据库
		model.RecordErrorLog(c, userId, channelId, modelName, tokenName, err.MaskSensitiveError(), tokenId, 0, false, userGroup, other)
	}

}

// RelayMidjourney 处理 Midjourney 图像生成代理请求
// 参数:
//
//	c: Gin 上下文对象
//
// 说明:
//
//	支持多种 Midjourney 操作模式:
//	- 通知回调
//	- 任务查询
//	- 图片种子获取
//	- 换脸
//	- 任务提交
func RelayMidjourney(c *gin.Context) {
	// 生成 Midjourney 代理请求的元信息
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatMjProxy, nil, nil)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"description": fmt.Sprintf("failed to generate relay info: %s", err.Error()),
			"type":        "upstream_error",
			"code":        4,
		})
		return
	}

	var mjErr *dto.MidjourneyResponse
	// 根据不同的代理模式调用对应的处理函数
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		// 通知回调
		mjErr = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		// 任务查询
		mjErr = relay.RelayMidjourneyTask(c, relayInfo.RelayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		// 图片种子获取
		mjErr = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		// 换脸功能
		mjErr = relay.RelaySwapFace(c, relayInfo)
	default:
		// 默认任务提交
		mjErr = relay.RelayMidjourneySubmit(c, relayInfo)
	}
	//err = relayMidjourneySubmit(c, relayMode)
	log.Println(mjErr)
	// 如果有错误，返回错误信息
	if mjErr != nil {
		statusCode := http.StatusBadRequest
		// 特殊处理：负载饱和错误
		if mjErr.Code == 30 {
			mjErr.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result),
			"type":        "upstream_error",
			"code":        mjErr.Code,
		})
		channelId := c.GetInt("channel_id")
		logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result)))
	}
}

// RelayNotImplemented 处理未实现的 API 请求
// 参数:
//
//	c: Gin 上下文对象
func RelayNotImplemented(c *gin.Context) {
	err := types.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

// RelayNotFound 处理找不到的 API 路径
// 参数:
//
//	c: Gin 上下文对象
func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTask(c *gin.Context) {
	retryTimes := common.RetryTimes
	channelId := c.GetInt("channel_id")
	c.Set("use_channel", []string{fmt.Sprintf("%d", channelId)})
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		return
	}
	taskErr := taskRelayHandler(c, relayInfo)
	if taskErr == nil {
		retryTimes = 0
	}
	retryParam := &service.RetryParam{
		Ctx:        c,
		TokenGroup: relayInfo.TokenGroup,
		ModelName:  relayInfo.OriginModelName,
		Retry:      common.GetPointer(0),
	}
	for ; shouldRetryTaskRelay(c, channelId, taskErr, retryTimes) && retryParam.GetRetry() < retryTimes; retryParam.IncreaseRetry() {
		channel, newAPIError := getChannel(c, relayInfo, retryParam)
		if newAPIError != nil {
			logger.LogError(c, fmt.Sprintf("CacheGetRandomSatisfiedChannel failed: %s", newAPIError.Error()))
			taskErr = service.TaskErrorWrapperLocal(newAPIError.Err, "get_channel_failed", http.StatusInternalServerError)
			break
		}
		channelId = channel.Id
		useChannel := c.GetStringSlice("use_channel")
		useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
		c.Set("use_channel", useChannel)
		logger.LogInfo(c, fmt.Sprintf("using channel #%d to retry (remain times %d)", channel.Id, retryParam.GetRetry()))
		//middleware.SetupContextForSelectedChannel(c, channel, originalModel)

		requestBody, _ := common.GetRequestBody(c)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		taskErr = taskRelayHandler(c, relayInfo)
	}
	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
	if taskErr != nil {
		if taskErr.StatusCode == http.StatusTooManyRequests {
			taskErr.Message = "当前分组上游负载已饱和，请稍后再试"
		}
		c.JSON(taskErr.StatusCode, taskErr)
	}
}

func taskRelayHandler(c *gin.Context, relayInfo *relaycommon.RelayInfo) *dto.TaskError {
	var err *dto.TaskError
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeSunoFetch, relayconstant.RelayModeSunoFetchByID, relayconstant.RelayModeVideoFetchByID:
		err = relay.RelayTaskFetch(c, relayInfo.RelayMode)
	default:
		err = relay.RelayTaskSubmit(c, relayInfo)
	}
	return err
}

func shouldRetryTaskRelay(c *gin.Context, channelId int, taskErr *dto.TaskError, retryTimes int) bool {
	if taskErr == nil {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if taskErr.StatusCode == 307 {
		return true
	}
	if taskErr.StatusCode/100 == 5 {
		// 超时不重试
		if taskErr.StatusCode == 504 || taskErr.StatusCode == 524 {
			return false
		}
		return true
	}
	if taskErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if taskErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if taskErr.LocalError {
		return false
	}
	if taskErr.StatusCode/100 == 2 {
		return false
	}
	return true
}

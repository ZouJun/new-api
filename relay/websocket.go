// Package relay 是 New API 的核心代理模块
package relay

import (
	"fmt"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// WssHelper 处理 WebSocket 流式连接，用于实时对话 API
// 参数:
//
//	c: Gin 上下文对象，包含请求信息
//	info: 代理请求的元信息，包括渠道、模型等信息
//
// 返回值:
//
//	*types.NewAPIError: 如果发生错误则返回错误对象，成功则返回 nil
//
// 说明:
//  1. 初始化渠道元信息
//  2. 获取并初始化对应的API适配器
//  3. 建立与上游服务的 WebSocket 连接
//  4. 处理响应流并转发给客户端
//  5. 统计使用量并扣除配额
func WssHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	// 初始化渠道元信息（模型、密钥等）
	info.InitChannelMeta(c)

	// 根据 API 类型获取对应的适配器
	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	// 初始化适配器
	adaptor.Init(info)
	//var requestBody io.Reader
	//firstWssRequest, _ := c.Get("first_wss_request")
	//requestBody = bytes.NewBuffer(firstWssRequest.([]byte))

	// 获取状态码映射配置，用于自定义错误状态码
	statusCodeMappingStr := c.GetString("status_code_mapping")
	// 发起对上游 WebSocket 服务的连接请求
	resp, err := adaptor.DoRequest(c, info, nil)
	if err != nil {
		return types.NewError(err, types.ErrorCodeDoRequestFailed)
	}

	// 如果成功建立连接，保存 WebSocket 连接并确保最后关闭
	if resp != nil {
		info.TargetWs = resp.(*websocket.Conn)
		defer info.TargetWs.Close()
	}

	// 处理 WebSocket 响应流，将消息转发给客户端，并统计使用量
	usage, newAPIError := adaptor.DoResponse(c, nil, info)
	if newAPIError != nil {
		// 如果发生错误，根据状态码映射重置错误状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}
	// 根据实际使用量扣除用户配额
	service.PostWssConsumeQuota(c, info, info.UpstreamModelName, usage.(*dto.RealtimeUsage), "")
	return nil
}

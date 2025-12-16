// Package middleware 包含所有 HTTP 中间件
// 负责请求预处理、验证、权限控制等
package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// validUserInfo 验证用户信息是否有效
// 参数:
//
//	username: 用户名
//	role: 用户角色
//
// 返回值:
//
//	bool: true 表示有效，false 表示无效
func validUserInfo(username string, role int) bool {
	// 检查用户名是否为空
	if strings.TrimSpace(username) == "" {
		return false
	}
	// 检查角色是否合法
	if !common.IsValidateRole(role) {
		return false
	}
	return true
}

// authHelper 是验证用户身份的核心辅助函数
// 参数:
//
//	c: Gin 上下文对象
//	minRole: 最小需要的角色等级
//
// 说明:
//  1. 优先检查 Session 中的用户信息
//  2. 如果 Session 不存在，检查 Authorization Header 中的 Access Token
//  3. 验证用户状态和权限
//  4. 将用户信息写入上下文
func authHelper(c *gin.Context, minRole int) {
	// 从 Session 中获取用户信息
	session := sessions.Default(c)
	username := session.Get("username")
	role := session.Get("role")
	id := session.Get("id")
	status := session.Get("status")
	useAccessToken := false
	// 如果 Session 中没有用户名，尝试使用 Access Token
	if username == nil {
		// 检查 Access Token
		accessToken := c.Request.Header.Get("Authorization")
		if accessToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"message": "无权进行此操作，未登录且未提供 access token",
			})
			c.Abort()
			return
		}
		// 验证 Access Token
		user := model.ValidateAccessToken(accessToken)
		if user != nil && user.Username != "" {
			if !validUserInfo(user.Username, user.Role) {
				c.JSON(http.StatusOK, gin.H{
					"success": false,
					"message": "无权进行此操作，用户信息无效",
				})
				c.Abort()
				return
			}
			// Token 有效，使用从 Token 解析出的用户信息
			username = user.Username
			role = user.Role
			id = user.Id
			status = user.Status
			useAccessToken = true
		} else {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无权进行此操作，access token 无效",
			})
			c.Abort()
			return
		}
	}
	// 从 Header 中获取 New-Api-User，用于验证请求的用户 ID
	apiUserIdStr := c.Request.Header.Get("New-Api-User")
	if apiUserIdStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "无权进行此操作，未提供 New-Api-User",
		})
		c.Abort()
		return
	}
	apiUserId, err := strconv.Atoi(apiUserIdStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "无权进行此操作，New-Api-User 格式错误",
		})
		c.Abort()
		return

	}
	// 验证请求的用户 ID 与当前登录用户 ID 是否匹配
	if id != apiUserId {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "无权进行此操作，New-Api-User 与登录用户不匹配",
		})
		c.Abort()
		return
	}
	// 检查用户状态是否被封禁
	if status.(int) == common.UserStatusDisabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "用户已被封禁",
		})
		c.Abort()
		return
	}
	// 检查用户角色是否满足最小权限要求
	if role.(int) < minRole {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无权进行此操作，权限不足",
		})
		c.Abort()
		return
	}
	// 再次验证用户信息是否有效
	if !validUserInfo(username.(string), role.(int)) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无权进行此操作，用户信息无效",
		})
		c.Abort()
		return
	}
	// 将用户信息设置到上下文中，供后续业务使用
	c.Set("username", username)
	c.Set("role", role)
	c.Set("id", id)
	c.Set("group", session.Get("group"))
	c.Set("user_group", session.Get("group"))
	c.Set("use_access_token", useAccessToken)

	//userCache, err := model.GetUserCache(id.(int))
	//if err != nil {
	//	c.JSON(http.StatusOK, gin.H{
	//		"success": false,
	//		"message": err.Error(),
	//	})
	//	c.Abort()
	//	return
	//}
	//userCache.WriteContext(c)

	// 继续执行后续处理函数
	c.Next()
}

// TryUserAuth 尝试获取用户身份，但不强制要求登录
// 返回值:
//
//	中间件函数
func TryUserAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		session := sessions.Default(c)
		id := session.Get("id")
		if id != nil {
			c.Set("id", id)
		}
		c.Next()
	}
}

// UserAuth 要求用户必须登录（普通用户及以上权限）
// 返回值:
//
//	中间件函数
func UserAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, common.RoleCommonUser)
	}
}

// AdminAuth 要求用户必须是管理员权限
// 返回值:
//
//	中间件函数
func AdminAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, common.RoleAdminUser)
	}
}

// RootAuth 要求用户必须是超级管理员权限
// 返回值:
//
//	中间件函数
func RootAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, common.RoleRootUser)
	}
}

// WssAuth WebSocket 连接验证（待实现）
// 参数:
//
//	c: Gin 上下文对象
func WssAuth(c *gin.Context) {

}

// TokenAuth 验证 API Token（sk-xxx 格式）
// 返回值:
//
//	中间件函数
//
// 说明:
//  1. 支持从多个来源解析 Token：Authorization Header、WebSocket Protocol、Anthropic Header、Gemini Query/Header
//  2. 验证 Token 有效性、IP 限制、用户状态
//  3. 验证分组权限
func TokenAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		// 先检测是否临 WebSocket 连接
		if c.Request.Header.Get("Sec-WebSocket-Protocol") != "" {
			// Sec-WebSocket-Protocol: realtime, openai-insecure-api-key.sk-xxx, openai-beta.realtime-v1
			// 从 Sec-WebSocket-Protocol 中读取 sk
			key := c.Request.Header.Get("Sec-WebSocket-Protocol")
			parts := strings.Split(key, ",")
			for _, part := range parts {
				part = strings.TrimSpace(part)
				if strings.HasPrefix(part, "openai-insecure-api-key") {
					key = strings.TrimPrefix(part, "openai-insecure-api-key.")
					break
				}
			}
			c.Request.Header.Set("Authorization", "Bearer "+key)
		}
		// 检查path包含/v1/messages
		if strings.Contains(c.Request.URL.Path, "/v1/messages") {
			anthropicKey := c.Request.Header.Get("x-api-key")
			if anthropicKey != "" {
				c.Request.Header.Set("Authorization", "Bearer "+anthropicKey)
			}
		}
		// Gemini API 从 query 中获取 key
		if strings.HasPrefix(c.Request.URL.Path, "/v1beta/models") ||
			strings.HasPrefix(c.Request.URL.Path, "/v1beta/openai/models") ||
			strings.HasPrefix(c.Request.URL.Path, "/v1/models/") {
			skKey := c.Query("key")
			if skKey != "" {
				c.Request.Header.Set("Authorization", "Bearer "+skKey)
			}
			// 从 x-goog-api-key header 中获取 key
			xGoogKey := c.Request.Header.Get("x-goog-api-key")
			if xGoogKey != "" {
				c.Request.Header.Set("Authorization", "Bearer "+xGoogKey)
			}
		}
		// 解析 Authorization Header 中的 Token
		key := c.Request.Header.Get("Authorization")
		parts := make([]string, 0)
		key = strings.TrimPrefix(key, "Bearer ")
		if key == "" || key == "midjourney-proxy" {
			key = c.Request.Header.Get("mj-api-secret")
			key = strings.TrimPrefix(key, "Bearer ")
			key = strings.TrimPrefix(key, "sk-")
			parts = strings.Split(key, "-")
			key = parts[0]
		} else {
			key = strings.TrimPrefix(key, "sk-")
			parts = strings.Split(key, "-")
			key = parts[0]
		}
		// 验证 Token 是否有效
		token, err := model.ValidateUserToken(key)
		if token != nil {
			id := c.GetInt("id")
			if id == 0 {
				c.Set("id", token.UserId)
			}
		}
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusUnauthorized, err.Error())
			return
		}

		// 检查 IP 白名单限制
		allowIpsMap := token.GetIpLimitsMap()
		if len(allowIpsMap) != 0 {
			clientIp := c.ClientIP()
			if _, ok := allowIpsMap[clientIp]; !ok {
				abortWithOpenAiMessage(c, http.StatusForbidden, "您的 IP 不在令牌允许访问的列表中")
				return
			}
		}

		// 获取用户缓存信息
		userCache, err := model.GetUserCache(token.UserId)
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusInternalServerError, err.Error())
			return
		}
		// 检查用户是否启用
		userEnabled := userCache.Status == common.UserStatusEnabled
		if !userEnabled {
			abortWithOpenAiMessage(c, http.StatusForbidden, "用户已被封禁")
			return
		}

		// 将用户信息写入上下文
		userCache.WriteContext(c)

		// 处理分组权限
		userGroup := userCache.Group
		tokenGroup := token.Group
		if tokenGroup != "" {
			// check common.UserUsableGroups[userGroup]
			if _, ok := service.GetUserUsableGroups(userGroup)[tokenGroup]; !ok {
				abortWithOpenAiMessage(c, http.StatusForbidden, fmt.Sprintf("无权访问 %s 分组", tokenGroup))
				return
			}
			// 检查分组是否在 GroupRatio 中
			if !ratio_setting.ContainsGroupRatio(tokenGroup) {
				if tokenGroup != "auto" {
					abortWithOpenAiMessage(c, http.StatusForbidden, fmt.Sprintf("分组 %s 已被弃用", tokenGroup))
					return
				}
			}
			userGroup = tokenGroup
		}
		// 设置使用的分组到上下文
		common.SetContextKey(c, constant.ContextKeyUsingGroup, userGroup)

		// 设置 Token 相关上下文信息
		err = SetupContextForToken(c, token, parts...)
		if err != nil {
			return
		}
		c.Next()
	}
}

// SetupContextForToken 将 Token 相关信息设置到上下文中
// 参数:
//
//	c: Gin 上下文对象
//	token: Token 对象
//	parts: Token 分割后的部分（用于指定渠道）
//
// 返回值:
//
//	error: 如果设置失败则返回错误
func SetupContextForToken(c *gin.Context, token *model.Token, parts ...string) error {
	if token == nil {
		return fmt.Errorf("token is nil")
	}
	// 设置基本 Token 信息
	c.Set("id", token.UserId)
	c.Set("token_id", token.Id)
	c.Set("token_key", token.Key)
	c.Set("token_name", token.Name)
	c.Set("token_unlimited_quota", token.UnlimitedQuota)
	// 如果不是无限配额，设置剩余配额
	if !token.UnlimitedQuota {
		c.Set("token_quota", token.RemainQuota)
	}
	// 如果启用了模型限制，设置模型限制信息
	if token.ModelLimitsEnabled {
		c.Set("token_model_limit_enabled", true)
		c.Set("token_model_limit", token.GetModelLimitsMap())
	} else {
		c.Set("token_model_limit_enabled", false)
	}
	// 设置 Token 的分组和跨组重试配置
	common.SetContextKey(c, constant.ContextKeyTokenGroup, token.Group)
	common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, token.CrossGroupRetry)
	// 如果指定了渠道 ID（仅管理员可用）
	if len(parts) > 1 {
		if model.IsAdmin(token.UserId) {
			c.Set("specific_channel_id", parts[1])
		} else {
			abortWithOpenAiMessage(c, http.StatusForbidden, "普通用户不支持指定渠道")
			return fmt.Errorf("普通用户不支持指定渠道")
		}
	}
	return nil
}

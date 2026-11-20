package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/xiaoqi/mini-custody/backend/internal/auth"
	"github.com/xiaoqi/mini-custody/backend/internal/logging"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

type authContextKey string

const (
	adminClaimsKey authContextKey = "admin_claims"
	// dummyPasswordHash 用于账号不存在时执行等耗时 bcrypt 校验，防止通过响应时间枚举管理员账号。
	dummyPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
)

// loginAttemptState 记录单个 IP+用户名 维度的令牌桶限流和连续失败锁定状态。
type loginAttemptState struct {
	tokens      float64
	lastRefill  time.Time
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

// AuthStore 定义管理员登录、身份查询与托管用户管理所需的数据访问能力。
type AuthStore interface {
	AdminByUsername(ctx context.Context, username string) (postgres.AdminUser, error)
	AdminByID(ctx context.Context, id int64) (postgres.AdminUser, error)
	UpdateAdminLastLogin(ctx context.Context, id int64) error
	CreateManagedUser(ctx context.Context, code, displayName, note string, evmProvider wallet.KeyProvider, btcProvider *wallet.MnemonicKeyProvider) (postgres.User, postgres.WalletAddress, error)
	UpdateManagedUser(ctx context.Context, id int64, displayName, note, status string) (postgres.User, error)
}

// loginRequest 描述管理员登录请求的 JSON 结构。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// adminProfileResponse 描述可安全返回的管理员身份信息。
type adminProfileResponse struct {
	ID          int64      `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Role        string     `json:"role"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// loginResponse 描述管理员登录成功响应。
type loginResponse struct {
	Token     string               `json:"token"`
	ExpiresAt time.Time            `json:"expires_at"`
	Admin     adminProfileResponse `json:"admin"`
}

// createUserRequest 描述后台新建托管用户的 JSON 请求体。
type createUserRequest struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
	Note        string `json:"note"`
}

// updateUserRequest 描述后台更新托管用户资料与状态的 JSON 请求体。
type updateUserRequest struct {
	DisplayName string `json:"display_name"`
	Note        string `json:"note"`
	Status      string `json:"status"`
}

// createUserResponse 描述新建托管用户及其初始 EVM 充值地址响应。
type createUserResponse struct {
	User    userResponse `json:"user"`
	Address string       `json:"address"`
}

// login 验证管理员账号密码并签发 HS256 JWT，包含限流、连续失败锁定和防枚举统一响应。
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if a.authStore == nil || len(a.jwtSecret) < 32 {
		writeAPIError(w, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "认证服务暂不可用")
		return
	}
	var req loginRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeAPIError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", "用户名和密码不能为空")
		return
	}
	now := time.Now().In(a.timezoneOrUTC())
	guardKey := loginGuardKey(r, username)
	if locked, retryAfter, rateLimited := a.checkLoginQuota(guardKey, now); locked {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeAPIError(w, http.StatusTooManyRequests, "ACCOUNT_LOCKED", "登录失败次数过多，请稍后再试")
		return
	} else if rateLimited {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeAPIError(w, http.StatusTooManyRequests, "RATE_LIMITED", "登录请求过于频繁，请稍后重试")
		return
	}

	admin, err := a.authStore.AdminByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			_ = auth.VerifyPassword(dummyPasswordHash, req.Password)
			a.recordLoginFailure(guardKey, now)
			writeAPIError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码错误")
			return
		}
		logging.WithContext(a.logger, r.Context()).Error("查询管理员账户失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		return
	}
	// 先校验密码再判断禁用状态，并统一返回 INVALID_CREDENTIALS 避免暴露账号存在性或启用状态。
	if !auth.VerifyPassword(admin.PasswordHash, req.Password) || admin.Disabled {
		a.recordLoginFailure(guardKey, now)
		writeAPIError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码错误")
		return
	}
	a.resetLoginFailures(guardKey)

	expiresAt := now.Add(a.config.Auth.JWTTTL)
	token, err := auth.SignToken(a.jwtSecret, auth.Claims{
		AdminID:  admin.ID,
		Username: admin.Username,
		Role:     admin.Role,
		PwdFP:    auth.PasswordFingerprint(admin.PasswordHash),
		IssuedAt: now.Unix(),
		ExpireAt: expiresAt.Unix(),
	})
	if err != nil {
		logging.WithContext(a.logger, r.Context()).Error("签发管理员 JWT 失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		return
	}
	_ = a.authStore.UpdateAdminLastLogin(r.Context(), admin.ID)
	writeJSON(w, http.StatusOK, loginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		Admin: adminProfileResponse{
			ID:          admin.ID,
			Username:    admin.Username,
			DisplayName: admin.DisplayName,
			Role:        admin.Role,
			LastLoginAt: admin.LastLoginAt,
		},
	})
}

// me 返回当前已认证管理员信息。
func (a *App) me(w http.ResponseWriter, r *http.Request) {
	claims, ok := r.Context().Value(adminClaimsKey).(auth.Claims)
	if !ok || (claims.AdminID <= 0 && strings.TrimSpace(claims.Username) == "") {
		writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "需要有效的管理员凭证")
		return
	}
	if a.authStore == nil || claims.AdminID <= 0 {
		writeJSON(w, http.StatusOK, adminProfileResponse{
			ID: claims.AdminID, Username: claims.Username, DisplayName: claims.Username, Role: claims.Role,
		})
		return
	}
	admin, err := a.authStore.AdminByID(r.Context(), claims.AdminID)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "管理员账号不存在")
			return
		}
		logging.WithContext(a.logger, r.Context()).Error("读取当前管理员失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		return
	}
	if admin.Disabled {
		writeAPIError(w, http.StatusForbidden, "ADMIN_DISABLED", "管理员账号已被禁用")
		return
	}
	writeJSON(w, http.StatusOK, adminProfileResponse{
		ID:          admin.ID,
		Username:    admin.Username,
		DisplayName: admin.DisplayName,
		Role:        admin.Role,
		LastLoginAt: admin.LastLoginAt,
	})
}

// createUser 创建新的托管用户并原子派生链上充值地址。
func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	if a.authStore == nil || a.keys == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "USER_CREATE_UNAVAILABLE", "开户服务暂不可用")
		return
	}
	var req createUserRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	var btcProvider *wallet.MnemonicKeyProvider
	if a.config.Bitcoin.Enabled {
		if mnemonicProvider, ok := a.keys.(*wallet.MnemonicKeyProvider); ok {
			btcProvider = mnemonicProvider
		}
	}
	user, walletRow, err := a.authStore.CreateManagedUser(r.Context(), req.Code, req.DisplayName, req.Note, a.keys, btcProvider)
	if err != nil {
		switch {
		case errors.Is(err, postgres.ErrInvalidUserInput):
			writeAPIError(w, http.StatusBadRequest, "INVALID_USER", "用户编码必须为 3-64 位小写字母开头的字母数字下划线或短横线，且展示名称不能为空")
		case errors.Is(err, postgres.ErrUserAlreadyExists):
			writeAPIError(w, http.StatusConflict, "USER_EXISTS", "该用户编码已被占用")
		default:
			logging.WithContext(a.logger, r.Context()).Error("创建托管用户失败", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		}
		return
	}
	writeJSON(w, http.StatusCreated, createUserResponse{
		User: userResponse{
			ID:          user.ID,
			Code:        user.Code,
			DisplayName: user.DisplayName,
			Status:      user.Status,
			Note:        user.Note,
			CreatedAt:   user.CreatedAt,
		},
		Address: common.HexToAddress(walletRow.Address).Hex(),
	})
}

// updateUser 更新托管用户的名称、备注与启用状态。
func (a *App) updateUser(w http.ResponseWriter, r *http.Request) {
	if a.authStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "USER_UPDATE_UNAVAILABLE", "用户管理服务暂不可用")
		return
	}
	userID, ok := parseUserID(w, r)
	if !ok {
		return
	}
	var req updateUserRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	user, err := a.authStore.UpdateManagedUser(r.Context(), userID, req.DisplayName, req.Note, req.Status)
	if err != nil {
		switch {
		case errors.Is(err, postgres.ErrNotFound):
			writeAPIError(w, http.StatusNotFound, "USER_NOT_FOUND", "用户不存在")
		case errors.Is(err, postgres.ErrInvalidUserInput):
			writeAPIError(w, http.StatusBadRequest, "INVALID_USER", "展示名称不能为空且状态必须是 ACTIVE 或 DISABLED")
		default:
			logging.WithContext(a.logger, r.Context()).Error("更新托管用户失败", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		}
		return
	}
	writeJSON(w, http.StatusOK, userResponse{
		ID:          user.ID,
		Code:        user.Code,
		DisplayName: user.DisplayName,
		Status:      user.Status,
		Note:        user.Note,
		CreatedAt:   user.CreatedAt,
	})
}

// requireAdminAuth 校验管理后台请求携带的 Bearer JWT 或可选自动化 X-API-Key，并在管理员禁用后立即使令牌失效。
func (a *App) requireAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.config.Auth.APIKey != "" {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey != "" && subtle.ConstantTimeCompare([]byte(apiKey), []byte(a.config.Auth.APIKey)) == 1 {
				username := strings.TrimSpace(a.config.Auth.APIKeyUsername)
				if username == "" {
					username = "system-api-key"
				}
				adminID := a.config.Auth.APIKeyAdminID
				if adminID > 0 && a.authStore != nil {
					admin, err := a.authStore.AdminByID(r.Context(), adminID)
					if err != nil || admin.Disabled {
						writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "API Key 绑定的管理员账号无效或已被禁用")
						return
					}
					username = admin.Username
				}
				ctx := context.WithValue(r.Context(), adminClaimsKey, auth.Claims{
					AdminID:  adminID,
					Username: username,
					Role:     postgres.AdminRole,
				})
				next(w, r.WithContext(ctx))
				return
			}
		}

		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(header, "Bearer ") {
			writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "请先登录管理后台")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		claims, err := auth.VerifyToken(a.jwtSecret, token, time.Now())
		if err != nil {
			code := "UNAUTHORIZED"
			message := "登录凭证无效，请重新登录"
			if errors.Is(err, auth.ErrExpiredToken) {
				code = "TOKEN_EXPIRED"
				message = "登录已过期，请重新登录"
			}
			writeAPIError(w, http.StatusUnauthorized, code, message)
			return
		}
		if a.authStore != nil {
			admin, dbErr := a.authStore.AdminByID(r.Context(), claims.AdminID)
			if errors.Is(dbErr, postgres.ErrNotFound) || (dbErr == nil && admin.Disabled) ||
				(dbErr == nil && claims.PwdFP != "" && claims.PwdFP != auth.PasswordFingerprint(admin.PasswordHash)) {
				writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "登录凭证已失效，请重新登录")
				return
			}
			if dbErr != nil {
				logging.WithContext(a.logger, r.Context()).Error("校验管理员实时状态失败", "error", dbErr)
				writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
				return
			}
		}
		ctx := context.WithValue(r.Context(), adminClaimsKey, claims)
		next(w, r.WithContext(ctx))
	}
}

// timezoneOrUTC 返回配置时区，测试未注入时区时安全回退为 UTC。
func (a *App) timezoneOrUTC() *time.Location {
	if a.config.Timezone != nil {
		return a.config.Timezone
	}
	return time.UTC
}

// loginPolicyParams 返回生效的登录限流与失败锁定参数，测试直接构造 App 时自动回退安全默认值。
func (a *App) loginPolicyParams() (rate float64, burst int, maxFailures int, lockDuration time.Duration) {
	rate = a.config.Auth.LoginRateLimitPerSec
	if rate <= 0 {
		rate = 5
	}
	burst = a.config.Auth.LoginRateLimitBurst
	if burst <= 0 {
		burst = 10
	}
	maxFailures = a.config.Auth.LoginMaxFailedAttempts
	if maxFailures <= 0 {
		maxFailures = 5
	}
	lockDuration = a.config.Auth.LoginLockDuration
	if lockDuration <= 0 {
		lockDuration = 15 * time.Minute
	}
	return rate, burst, maxFailures, lockDuration
}

// loginGuardKey 构造按来源 IP 与归一化用户名隔离的限流键。
func loginGuardKey(r *http.Request, username string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil || host == "" {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		host = "unknown"
	}
	return host + "|" + strings.ToLower(strings.TrimSpace(username))
}

// checkLoginQuota 检查账号是否处于锁定窗口或触发 IP+用户名 令牌桶限流。
func (a *App) checkLoginQuota(key string, now time.Time) (locked bool, retryAfterSec int, rateLimited bool) {
	rate, burst, _, _ := a.loginPolicyParams()
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.loginStates == nil {
		a.loginStates = make(map[string]*loginAttemptState)
	}
	if len(a.loginStates) > 1000 {
		for k, item := range a.loginStates {
			if now.Sub(item.lastSeen) > time.Hour && !now.Before(item.lockedUntil) {
				delete(a.loginStates, k)
			}
		}
	}
	state, ok := a.loginStates[key]
	if !ok {
		state = &loginAttemptState{tokens: float64(burst), lastRefill: now, lastSeen: now}
		a.loginStates[key] = state
	}
	state.lastSeen = now
	if !state.lockedUntil.IsZero() && now.Before(state.lockedUntil) {
		sec := int(state.lockedUntil.Sub(now).Seconds()) + 1
		if sec < 1 {
			sec = 1
		}
		return true, sec, false
	}
	if !state.lockedUntil.IsZero() && !now.Before(state.lockedUntil) {
		state.lockedUntil = time.Time{}
		state.failures = 0
	}
	elapsed := now.Sub(state.lastRefill).Seconds()
	if elapsed > 0 {
		state.tokens += elapsed * rate
		if state.tokens > float64(burst) {
			state.tokens = float64(burst)
		}
		state.lastRefill = now
	}
	if state.tokens < 1.0 {
		return false, 1, true
	}
	state.tokens -= 1.0
	return false, 0, false
}

// recordLoginFailure 累加连续失败次数并在达到阈值后锁定该 IP+用户名 组合。
func (a *App) recordLoginFailure(key string, now time.Time) {
	_, burst, maxFailures, lockDuration := a.loginPolicyParams()
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.loginStates == nil {
		a.loginStates = make(map[string]*loginAttemptState)
	}
	state, ok := a.loginStates[key]
	if !ok {
		state = &loginAttemptState{tokens: float64(burst), lastRefill: now, lastSeen: now}
		a.loginStates[key] = state
	}
	state.lastSeen = now
	state.failures++
	if state.failures >= maxFailures {
		state.lockedUntil = now.Add(lockDuration)
	}
}

// resetLoginFailures 在登录成功后清除该 IP+用户名 组合的失败计数与锁定状态。
func (a *App) resetLoginFailures(key string) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if state, ok := a.loginStates[key]; ok {
		state.failures = 0
		state.lockedUntil = time.Time{}
	}
}

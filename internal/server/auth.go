package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/zxcll/vps-panel/internal/store"
)

// tokenTTL 是登录态有效期。
const tokenTTL = 7 * 24 * time.Hour

// Bootstrap 确保管理员账号和 JWT 密钥存在。
// 首次启动时生成随机密码并返回，由调用方打印到控制台。
// 返回空串表示账号早就建好了。
func Bootstrap(ctx context.Context, st *store.Store) (initialPassword string, err error) {
	if secret, _ := st.GetSetting(ctx, store.KeyJWTSecret); secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("生成 JWT 密钥: %w", err)
		}
		if err := st.SetSetting(ctx, store.KeyJWTSecret, hex.EncodeToString(b)); err != nil {
			return "", err
		}
	}

	if user, _ := st.GetSetting(ctx, store.KeyAdminUser); user == "" {
		if err := st.SetSetting(ctx, store.KeyAdminUser, "admin"); err != nil {
			return "", err
		}
	}

	hash, _ := st.GetSetting(ctx, store.KeyAdminPassHash)
	if hash != "" {
		return "", nil
	}

	pw, err := randomPassword()
	if err != nil {
		return "", err
	}
	if err := setPassword(ctx, st, pw); err != nil {
		return "", err
	}
	return pw, nil
}

func randomPassword() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机密码: %w", err)
	}
	// URL 安全字符集，避免用户复制粘贴时被转义搞乱
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func setPassword(ctx context.Context, st *store.Store, pw string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("计算密码哈希: %w", err)
	}
	return st.SetSetting(ctx, store.KeyAdminPassHash, string(h))
}

func (s *Server) jwtSecret(ctx context.Context) ([]byte, error) {
	v, err := s.st.GetSetting(ctx, store.KeyJWTSecret)
	if err != nil {
		return nil, err
	}
	if v == "" {
		return nil, errors.New("JWT 密钥未初始化")
	}
	return []byte(v), nil
}

func (s *Server) issueToken(ctx context.Context, user string) (string, time.Time, error) {
	secret, err := s.jwtSecret(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(tokenTTL)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   user,
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(exp),
	})
	signed, err := tok.SignedString(secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("签发登录令牌: %w", err)
	}
	return signed, exp, nil
}

func (s *Server) verifyToken(ctx context.Context, tokenStr string) (string, error) {
	secret, err := s.jwtSecret(ctx)
	if err != nil {
		return "", err
	}
	var claims jwt.RegisteredClaims
	_, err = jwt.ParseWithClaims(tokenStr, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("非预期的签名算法 %v", t.Header["alg"])
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Username  string    `json:"username"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ctx := r.Context()
	user, _ := s.st.GetSetting(ctx, store.KeyAdminUser)
	hash, _ := s.st.GetSetting(ctx, store.KeyAdminPassHash)

	// 无论用户名对不对都跑一次 bcrypt，避免通过响应时间推断用户名是否存在
	pwErr := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password))
	if req.Username != user || pwErr != nil {
		s.log.Warn("登录失败", "username", req.Username, "remote", clientIP(r, s.trustProxy))
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	token, exp, err := s.issueToken(ctx, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.log.Info("登录成功", "username", user, "remote", clientIP(r, s.trustProxy))
	writeJSON(w, http.StatusOK, loginResponse{Token: token, ExpiresAt: exp, Username: user})
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
	NewUsername string `json:"new_username"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ctx := r.Context()
	hash, _ := s.st.GetSetting(ctx, store.KeyAdminPassHash)
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)); err != nil {
		writeError(w, http.StatusUnauthorized, "当前密码不正确")
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "新密码至少需要 8 位")
		return
	}

	if err := setPassword(ctx, s.st, req.NewPassword); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if u := strings.TrimSpace(req.NewUsername); u != "" {
		if err := s.st.SetSetting(ctx, store.KeyAdminUser, u); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	s.st.AddEvent(ctx, nil, store.EventSystem, store.LevelInfo, "管理员密码已修改")
	writeJSON(w, http.StatusOK, map[string]string{"message": "密码已更新，请重新登录"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _ := s.st.GetSetting(r.Context(), store.KeyAdminUser)
	writeJSON(w, http.StatusOK, map[string]any{"username": user})
}

// requireAuth 是需要登录的接口的中间件。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := ""
		if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
			token = strings.TrimSpace(after)
		}
		if token == "" {
			writeError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		if _, err := s.verifyToken(r.Context(), token); err != nil {
			writeError(w, http.StatusUnauthorized, "登录已过期，请重新登录")
			return
		}
		next(w, r)
	}
}

// clientIP 取客户端 IP，面板挂在反代后面时读 X-Forwarded-For。
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if rip := r.Header.Get("X-Real-IP"); rip != "" {
			return rip
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return host
}

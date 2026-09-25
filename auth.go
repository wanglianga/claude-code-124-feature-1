package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

type ctxKey string

const ctxUserKey ctxKey = "user"

// User 登录用户（session 中缓存的视图）
type User struct {
	ID          int     `json:"id"`
	Username    string  `json:"username"`
	Role        string  `json:"role"`
	Name        string  `json:"name"`
	CreditLimit float64 `json:"credit_limit"`
	CreditDays  int     `json:"credit_days"`
	CreditScore int     `json:"credit_score"`
}

func randToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func userOf(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUserKey).(*User)
	return u
}

// auth 校验 Bearer token（Redis 会话，8 小时有效）
func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(h, "Bearer ")
		if tok == "" || tok == h {
			fail(w, http.StatusUnauthorized, "未登录或缺少 token")
			return
		}
		val, err := a.rdb.Get(r.Context(), "sess:"+tok).Result()
		if err == redis.Nil {
			fail(w, http.StatusUnauthorized, "会话已过期，请重新登录")
			return
		} else if err != nil {
			fail(w, http.StatusInternalServerError, "会话校验失败: "+err.Error())
			return
		}
		var u User
		if err := json.Unmarshal([]byte(val), &u); err != nil {
			fail(w, http.StatusInternalServerError, "会话数据异常")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserKey, &u)
		next(w, r.WithContext(ctx))
	}
}

// require 角色鉴权
func (a *app) require(roles ...string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return a.auth(func(w http.ResponseWriter, r *http.Request) {
			u := userOf(r)
			for _, role := range roles {
				if u.Role == role {
					next(w, r)
					return
				}
			}
			fail(w, http.StatusForbidden, "当前角色无权操作")
		})
	}
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &in); err != nil || in.Username == "" || in.Password == "" {
		fail(w, http.StatusBadRequest, "请输入用户名和密码")
		return
	}
	var u User
	var hash string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT id, username, role, name, credit_limit, credit_days, credit_score FROM users WHERE username=$1`,
		in.Username).Scan(&u.ID, &u.Username, &u.Role, &u.Name, &u.CreditLimit, &u.CreditDays, &u.CreditScore)
	if err != nil {
		fail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	err = a.db.QueryRowContext(r.Context(), `SELECT password_hash FROM users WHERE id=$1`, u.ID).Scan(&hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		fail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	token := randToken()
	data, _ := json.Marshal(u)
	if err := a.rdb.Set(r.Context(), "sess:"+token, data, 8*time.Hour).Err(); err != nil {
		fail(w, http.StatusInternalServerError, "会话写入失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": u})
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	a.rdb.Del(r.Context(), "sess:"+tok)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, userOf(r))
}

// handleListFarmers 花农列表（经纪人代录批次、管理方查看排名用）
func (a *app) handleListFarmers(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, username, role, name, credit_limit, credit_days, credit_score FROM users WHERE role='farmer' ORDER BY credit_score DESC, id`)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Name, &u.CreditLimit, &u.CreditDays, &u.CreditScore); err == nil {
			out = append(out, u)
		}
	}
	writeJSON(w, 200, out)
}

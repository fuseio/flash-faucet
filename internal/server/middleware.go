package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v2"
	"github.com/kataras/hcaptcha"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/negroni/v3"
)

type Limiter struct {
	mutex      sync.Mutex
	cache      *ttlcache.Cache
	proxyCount int
	ttl        time.Duration
}

func NewLimiter(proxyCount int, ttl time.Duration) *Limiter {
	cache := ttlcache.NewCache()
	cache.SkipTTLExtensionOnHit(true)
	return &Limiter{
		cache:      cache,
		proxyCount: proxyCount,
		ttl:        ttl,
	}
}

func (l *Limiter) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	address, err := readAddress(r)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			renderJSON(w, claimResponse{Message: mr.message}, mr.status)
		} else {
			renderJSON(w, claimResponse{Message: http.StatusText(http.StatusInternalServerError)}, http.StatusInternalServerError)
		}
		return
	}

	if l.ttl <= 0 {
		next.ServeHTTP(w, r)
		return
	}

	clientIP := getClientIPFromRequest(l.proxyCount, r)
	l.mutex.Lock()
	if l.limitByKey(w, address) || l.limitByKey(w, clientIP) {
		l.mutex.Unlock()
		return
	}
	l.cache.SetWithTTL(address, true, l.ttl)
	l.cache.SetWithTTL(clientIP, true, l.ttl)
	l.mutex.Unlock()

	next.ServeHTTP(w, r)
	if w.(negroni.ResponseWriter).Status() != http.StatusOK {
		l.cache.Remove(address)
		l.cache.Remove(clientIP)
		return
	}
	log.WithFields(log.Fields{
		"address":  address,
		"clientIP": clientIP,
	}).Info("Maximum request limit has been reached")
}

func (l *Limiter) limitByKey(w http.ResponseWriter, key string) bool {
	if _, ttl, err := l.cache.GetWithTTL(key); err == nil {
		errMsg := fmt.Sprintf("You have exceeded the rate limit. Please wait %s before you try again", ttl.Round(time.Second))
		renderJSON(w, claimResponse{Message: errMsg}, http.StatusTooManyRequests)
		return true
	}
	return false
}

func getClientIPFromRequest(proxyCount int, r *http.Request) string {
	if proxyCount > 0 {
		xForwardedFor := r.Header.Get("X-Forwarded-For")
		if xForwardedFor != "" {
			xForwardedForParts := strings.Split(xForwardedFor, ",")
			// Avoid reading the user's forged request header by configuring the count of reverse proxies
			partIndex := len(xForwardedForParts) - proxyCount
			if partIndex < 0 {
				partIndex = 0
			}
			return strings.TrimSpace(xForwardedForParts[partIndex])
		}
	}

	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}
	return remoteIP
}

type Captcha struct {
	client *hcaptcha.Client
	secret string
}

func NewCaptcha(hcaptchaSiteKey, hcaptchaSecret string) *Captcha {
	client := hcaptcha.New(hcaptchaSecret)
	client.SiteKey = hcaptchaSiteKey
	return &Captcha{
		client: client,
		secret: hcaptchaSecret,
	}
}

func (c *Captcha) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if c.secret == "" {
		next.ServeHTTP(w, r)
		return
	}

	response := c.client.VerifyToken(r.Header.Get("h-captcha-response"))
	if !response.Success {
		renderJSON(w, claimResponse{Message: "Captcha verification failed, please try again"}, http.StatusTooManyRequests)
		return
	}

	next.ServeHTTP(w, r)
}

func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// redis.go — minimal RESP (Redis serialization protocol) client used for the
// USSD session store when REDIS_URL is set (shared, multi-node sessions).
//
// Perf H8: the client keeps ONE persistent, mutex-multiplexed TCP connection
// instead of dialling a fresh connection per command (the old code did 3-5
// handshakes per USSD screen: Get + 1-2xPut + phone index). RESP is strictly
// request/response, so a single connection guarded by a mutex is correct and
// removes all per-command dial latency; on any I/O error the connection is
// dropped and re-established once before the command fails.

type redisClient struct {
	addr string
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

func dialRedis(addr string) (*redisClient, error) {
	c := &redisClient{addr: addr}
	if err := c.ping(); err != nil {
		return nil, err
	}
	return c, nil
}

// dialLocked (re)opens the persistent connection. Caller holds c.mu.
func (c *redisClient) dialLocked() error {
	conn, err := net.DialTimeout("tcp", c.addr, 3*time.Second)
	if err != nil {
		return err
	}
	c.conn = conn
	c.r = bufio.NewReader(conn)
	return nil
}

// closeLocked drops a broken connection so the next command redials.
func (c *redisClient) closeLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.r = nil
}

func (c *redisClient) cmd(args ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if c.conn == nil {
			if err := c.dialLocked(); err != nil {
				return "", err
			}
		}
		out, err := c.roundTripLocked(args...)
		if err == nil {
			return out, nil
		}
		// stale/half-open connection: drop it and retry once on a fresh dial
		lastErr = err
		c.closeLocked()
	}
	return "", fmt.Errorf("redis: command failed after reconnect: %w", lastErr)
}

func (c *redisClient) roundTripLocked(args ...string) (string, error) {
	_ = c.conn.SetDeadline(time.Now().Add(3 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		return "", err
	}
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "+"):
		return line[1:], nil
	case strings.HasPrefix(line, "-"):
		return "", fmt.Errorf("redis: %s", line[1:])
	case strings.HasPrefix(line, "$"):
		var n int
		fmt.Sscanf(line[1:], "%d", &n)
		if n < 0 {
			return "", fmt.Errorf("redis: nil")
		}
		buf := make([]byte, n+2)
		if _, err := readFull(c.r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	case strings.HasPrefix(line, ":"):
		return line[1:], nil
	}
	return "", fmt.Errorf("redis: unexpected reply %q", line)
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (c *redisClient) ping() error {
	_, err := c.cmd("PING")
	return err
}

// RedisSessionStore is the Redis-backed SessionStore (SETEX/GET) with a
// phone index key for resume. Used when REDIS_URL is set.
type RedisSessionStore struct {
	c   *redisClient
	ttl time.Duration
}

func NewRedisSessionStore(addr string, ttlSeconds int) (*RedisSessionStore, error) {
	c, err := dialRedis(addr)
	if err != nil {
		return nil, err
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 180
	}
	return &RedisSessionStore{c: c, ttl: time.Duration(ttlSeconds) * time.Second}, nil
}

func (s *RedisSessionStore) Get(id string) (*Session, bool) {
	v, err := s.c.cmd("GET", "ussd:sess:"+id)
	if err != nil {
		return nil, false
	}
	var sess Session
	if json.Unmarshal([]byte(v), &sess) != nil {
		return nil, false
	}
	sess.ExpiresAt = time.Now().Add(s.ttl)
	return &sess, true
}

func (s *RedisSessionStore) Put(sess *Session) {
	b, _ := json.Marshal(sess)
	ttlSec := int(s.ttl.Seconds())
	_, _ = s.c.cmd("SETEX", "ussd:sess:"+sess.ID, fmt.Sprint(ttlSec), string(b))
	if sess.Phone != "" {
		_, _ = s.c.cmd("SETEX", "ussd:phone:"+sess.Phone, fmt.Sprint(ttlSec), sess.ID)
	}
}

func (s *RedisSessionStore) Delete(id string) {
	_, _ = s.c.cmd("DEL", "ussd:sess:"+id)
}

func (s *RedisSessionStore) GetByPhone(phone string) (*Session, bool) {
	id, err := s.c.cmd("GET", "ussd:phone:"+phone)
	if err != nil || id == "" {
		return nil, false
	}
	return s.Get(id)
}

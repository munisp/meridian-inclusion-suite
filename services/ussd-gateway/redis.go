package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// redis.go — minimal RESP (Redis serialization protocol) client used for the
// USSD session store when REDIS_URL is set (shared, multi-node sessions).
// REAL code against the Redis protocol; tagged UNVERIFIED here because the
// test environment has no Redis server — the KV store is the tested default.

type redisClient struct{ addr string }

func dialRedis(addr string) (*redisClient, error) {
	c := &redisClient{addr: addr}
	if err := c.ping(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *redisClient) cmd(args ...string) (string, error) {
	conn, err := net.DialTimeout("tcp", c.addr, 3*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return "", err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
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
		if _, err := readFull(r, buf); err != nil {
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

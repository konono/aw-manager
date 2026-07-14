package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// SessionKey identifies a session by user and channel pair.
type SessionKey struct {
	UserID    string
	ChannelID string
}

func (k SessionKey) String() string {
	return k.UserID + ":" + k.ChannelID
}

// Session maps a user+channel pair to a running pod.
type Session struct {
	Key       SessionKey
	PodName   string
	Namespace string
}

// Store manages sessions in Redis with TTL-based expiration.
type Store struct {
	client      *redis.Client
	idleTimeout time.Duration
}

// NewStore connects to Redis and returns a session store.
func NewStore(redisURL string, idleTimeout time.Duration) (*Store, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parsing redis URL: %w", err)
	}

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("connecting to redis: %w", err)
	}

	return &Store{
		client:      client,
		idleTimeout: idleTimeout,
	}, nil
}

func (s *Store) Close() error {
	return s.client.Close()
}

// Ping checks Redis connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

func podKey(k SessionKey) string    { return "session:" + k.String() + ":pod" }
func nsKey(k SessionKey) string     { return "session:" + k.String() + ":ns" }
func activeKey(k SessionKey) string { return "session:" + k.String() + ":active" }

func (s *Store) GetSession(ctx context.Context, key SessionKey) (*Session, error) {
	podName, err := s.client.Get(ctx, podKey(key)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting session pod: %w", err)
	}

	ns, err := s.client.Get(ctx, nsKey(key)).Result()
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("getting session namespace: %w", err)
	}

	return &Session{
		Key:       key,
		PodName:   podName,
		Namespace: ns,
	}, nil
}

func (s *Store) SetSession(ctx context.Context, sess *Session) error {
	pipe := s.client.Pipeline()
	ttl := s.idleTimeout * 2

	pipe.Set(ctx, podKey(sess.Key), sess.PodName, ttl)
	pipe.Set(ctx, nsKey(sess.Key), sess.Namespace, ttl)
	pipe.Set(ctx, activeKey(sess.Key), time.Now().UTC().Format(time.RFC3339), ttl)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting session: %w", err)
	}
	return nil
}

func (s *Store) TouchSession(ctx context.Context, key SessionKey) error {
	ttl := s.idleTimeout * 2
	pipe := s.client.Pipeline()

	pipe.Expire(ctx, podKey(key), ttl)
	pipe.Expire(ctx, nsKey(key), ttl)
	pipe.Set(ctx, activeKey(key), time.Now().UTC().Format(time.RFC3339), ttl)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("touching session: %w", err)
	}
	return nil
}

func (s *Store) DeleteSession(ctx context.Context, key SessionKey) error {
	pipe := s.client.Pipeline()
	pipe.Del(ctx, podKey(key))
	pipe.Del(ctx, nsKey(key))
	pipe.Del(ctx, activeKey(key))

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

func (s *Store) GetLastActive(ctx context.Context, key SessionKey) (time.Time, error) {
	val, err := s.client.Get(ctx, activeKey(key)).Result()
	if err == redis.Nil {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("getting last active: %w", err)
	}

	t, err := time.Parse(time.RFC3339, val)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing last active time: %w", err)
	}
	return t, nil
}

func (s *Store) ListSessionKeys(ctx context.Context) ([]SessionKey, error) {
	var keys []SessionKey
	iter := s.client.Scan(ctx, 0, "session:*:pod", 100).Iterator()
	for iter.Next(ctx) {
		redisKey := iter.Val()
		// key format: session:<userID>:<channelID>:pod
		trimmed := strings.TrimPrefix(redisKey, "session:")
		trimmed = strings.TrimSuffix(trimmed, ":pod")
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) == 2 {
			keys = append(keys, SessionKey{UserID: parts[0], ChannelID: parts[1]})
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scanning sessions: %w", err)
	}
	return keys, nil
}

// Redis 通知/幂等后端（ADR-199）——NotificationStore + IdempotencyStore 实现。
//
// 依据: 09 §1 storage = Redis(缓存/消息域)；通知与幂等键必须跨重启存活（生产口径）。
// 键域: ca:notif:<id>=JSON；ca:notif:idx:<user_id>=ZSET(score=created_at UnixNano)；
//	ca:idem:<request_id>=JSON{body_hash}（TTL 24h 防重放窗口）。
//	ADR-210: 前缀 am:→ca:（auditmind→codeaudit）；存量 am:* 不迁移——幂等键 24h 自灭，
//	通知为可再生数据，部署后一次性 SCAN am:* 清理。
//
// 规模注记: ListNotifications 按 user 索引逐条取 JSON 后过滤 unread——通知量级
// （人读、非机器吞吐）下成立；索引级 unread 集合留待规模化。
package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	notifKeyPrefix = "ca:notif:"
	notifIdxPrefix = "ca:notif:idx:"
	idemKeyPrefix  = "ca:idem:"
	idemTTL        = 24 * time.Hour // 幂等重放窗口（03 §2 同键同体重放语义的保留期）
)

// RedisNotificationStore — Redis 实现。
type RedisNotificationStore struct {
	rdb *redis.Client
	ctx context.Context
}

// NewRedisNotificationStore — addr 形如 host:port；PING 探活失败即 fail-fast。
func NewRedisNotificationStore(addr string) (*RedisNotificationStore, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return &RedisNotificationStore{rdb: rdb, ctx: context.Background()}, nil
}

func notifKey(id string) string { return notifKeyPrefix + id }

// SaveNotification — JSON 落键 + 用户索引 ZADD。
func (r *RedisNotificationStore) SaveNotification(n *v1.Notification) {
	if n.CreatedAt == nil {
		n.CreatedAt = timestamppb.Now()
	}
	b, _ := json.Marshal(n)
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	pipe := r.rdb.Pipeline()
	pipe.Set(ctx, notifKey(n.NotificationId), b, 0)
	if n.UserId != "" {
		pipe.ZAdd(ctx, notifIdxPrefix+n.UserId, redis.Z{Score: float64(n.CreatedAt.AsTime().UnixNano()), Member: n.NotificationId})
	}
	_, _ = pipe.Exec(ctx)
}

// GetNotification — 按 id 取 JSON。
func (r *RedisNotificationStore) GetNotification(id string) (*v1.Notification, bool) {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	b, err := r.rdb.Get(ctx, notifKey(id)).Bytes()
	if err != nil {
		return nil, false
	}
	var n v1.Notification
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, false
	}
	return &n, true
}

// MarkRead — 读改写 read 标记（proto Notification.read 字段语义）。
func (r *RedisNotificationStore) MarkRead(id string) (*v1.Notification, bool) {
	n, ok := r.GetNotification(id)
	if !ok {
		return nil, false
	}
	n.Read = true
	r.SaveNotification(n)
	return n, true
}

// ListNotifications — 用户索引倒序 + unread 过滤。
func (r *RedisNotificationStore) ListNotifications(userID string, unreadOnly bool) []*v1.Notification {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	ids, err := r.rdb.ZRevRange(ctx, notifIdxPrefix+userID, 0, -1).Result()
	if err != nil {
		return nil
	}
	var out []*v1.Notification
	for _, id := range ids {
		if n, ok := r.GetNotification(id); ok {
			if unreadOnly && n.Read {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// idemEntry — 幂等键的序列化形态（response 统一 Empty；类型化缓存仅内存档需要）。
type idemEntry struct {
	BodyHash string `json:"body_hash"`
}

// CheckIdempotency — 同键比对 bodyHash：同体=命中重放；异体=AlreadyExists 语义。
func (r *RedisNotificationStore) CheckIdempotency(requestID, bodyHash string) (*IdempotencyEntry, bool, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	b, err := r.rdb.Get(ctx, idemKeyPrefix+requestID).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var e idemEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, false, err
	}
	if e.BodyHash != bodyHash {
		return nil, false, fmt.Errorf("request_id %s already used with a different body (03 §2)", requestID)
	}
	return &IdempotencyEntry{BodyHash: e.BodyHash, Response: &emptypb.Empty{}}, true, nil
}

// SetIdempotency — 记录 bodyHash（response 恒 Empty 语义；TTL=重放窗口）。
func (r *RedisNotificationStore) SetIdempotency(requestID, bodyHash string, response interface{}) {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	b, _ := json.Marshal(idemEntry{BodyHash: bodyHash})
	_ = r.rdb.Set(ctx, idemKeyPrefix+requestID, b, idemTTL).Err()
}

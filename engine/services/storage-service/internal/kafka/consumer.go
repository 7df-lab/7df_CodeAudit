// Package kafka — storage-service 的 Kafka 消费者（ADR-199 真实现，替代 ADR-135 stub）。
//
// 依据: 01 §4.3 五 topic 口径（task.created/task.completed/task.stage.completed/
// finding.created/finding.verdict.updated）；09 §2 行 notification(storage内) ← Kafka。
// 形态: 每 topic 一个 Reader（kafka-go 单 topic 消费组是经充分验证的路径；
// GroupTopics 多主题联合分配实测不拉取），同组 "storage-service" 各自独立位点。
// StartOffset=FirstOffset: 组位点缺失时从头回放——通知侧以事件幂等键兜底去重，
// 消费先于生产即等价于 latest；broker 数据保留期由服务端策略承担。
// 容错: 连接/消费错误指数化退避（初值 ADR-135 consumer_backoff_s，上限 60s），
// broker 缺席不拖垮服务进程——通知中心退化为"无新事件"而非不可用。
package kafka

import (
	"context"
	"log"
	"time"

	"github.com/segmentio/kafka-go"
)

// Topics consumed by storage-service (01 §4.3).
var ConsumedTopics = []string{
	"task.created",
	"task.completed",
	"task.stage.completed",
	"finding.created",
	"finding.verdict.updated",
}

// Dispatch — 单条消息的业务回调（topic + message key + payload JSON）。
type Dispatch func(topic string, key []byte, value []byte)

// Consumer — 真实 Kafka 消费者（消费组语义，broker 地址来自配置/env）。
type Consumer struct {
	brokers []string
	group   string
	topics  []string
	backoff time.Duration
}

// NewConsumer — group 固定 "storage-service"。
func NewConsumer(brokers []string, backoff time.Duration) *Consumer {
	return &Consumer{brokers: brokers, group: "storage-service", topics: ConsumedTopics, backoff: backoff}
}

// Start — 为每个 topic 起一个消费组 Reader（调用方置于 goroutine + ctx 取消）。
func (c *Consumer) Start(ctx context.Context, dispatch Dispatch) {
	for _, topic := range c.topics {
		go c.consume(ctx, topic, dispatch)
	}
}

func (c *Consumer) consume(ctx context.Context, topic string, dispatch Dispatch) {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     c.brokers,
		GroupID:     c.group,
		Topic:       topic,
		MinBytes:    1,
		MaxBytes:    10 << 20,          // 10MiB
		StartOffset: kafka.FirstOffset, // 组位点缺失时从头回放（通知侧事件幂等键兜底去重）
	})
	defer reader.Close()
	log.Printf("[kafka] consumer started: brokers=%v group=%s topic=%s", c.brokers, c.group, topic)

	failures := 0
	for {
		m, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("[kafka] %s consumer stopped: %v", topic, ctx.Err())
				return
			}
			failures++
			wait := c.backoff * time.Duration(failures)
			if wait > 60*time.Second {
				wait = 60 * time.Second
			}
			log.Printf("[kafka] %s read error #%d: %v (retry in %s)", topic, failures, err, wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		failures = 0
		dispatch(m.Topic, m.Key, m.Value)
	}
}

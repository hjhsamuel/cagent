package schema

import "time"

// Hello 是连接时拓扑探测的命令返回结构，不是业务集合。
type Hello struct {
	// SetName 是副本集名称；非空表示连接到副本集节点，用于验证事务拓扑。
	SetName string `bson:"setName"`
	// Msg 是服务端拓扑标识；值为 isdbgrid 时表示连接到 mongos 分片路由器。
	Msg string `bson:"msg"`
	// Sessions 是服务端逻辑会话超时分钟数；nil 表示未报告会话支持，适配器会拒绝该拓扑。
	Sessions *int64 `bson:"logicalSessionTimeoutMinutes"`
	// Primary 表示节点当前是否可接受写入；测试初始化副本集时用它等待主节点就绪。
	Primary bool `bson:"isWritablePrimary"`
}

// ClockTime 映射服务端时钟聚合的返回结果。
type ClockTime struct {
	// Now 是 MongoDB 聚合表达式 $$NOW 返回的服务端时间，用于生成存储时间和判断租约有效期。
	Now time.Time `bson:"now"`
}

// Count 映射测试故障注入命令的返回结果。
type Count struct {
	// Count 是 failpoint 命令返回的触发计数，用于验证故障注入是否执行。
	Count int64 `bson:"count"`
}

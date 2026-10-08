package schema

import "time"

// Hello 是连接时拓扑探测的命令返回结构，不是业务集合。
type Hello struct {
	SetName  string `bson:"setName"`
	Msg      string `bson:"msg"`
	Sessions *int64 `bson:"logicalSessionTimeoutMinutes"`
	Primary  bool   `bson:"isWritablePrimary"`
}

type ClockTime struct {
	Now time.Time `bson:"now"`
}
type Count struct {
	Count int64 `bson:"count"`
}

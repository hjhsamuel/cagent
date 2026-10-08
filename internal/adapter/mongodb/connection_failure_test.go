package mongodb

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestMongoConnectionLossDuringCommit 在服务器收到提交命令时真实断开 TCP。
// 与 writeConcernError 不同，这里客户端无法从响应判断提交结果；驱动必须重连，
// 存储回执保证上层重复提交不再次分配事件序号。仅允许操作本测试启动的临时实例，
// 不在外部共享 URI 上设置影响其他请求的全局 failpoint，也不使用 t.Parallel。
func TestMongoConnectionLossDuringCommit(t *testing.T) {
	if os.Getenv("CAGENT_TEST_MONGOD") == "" {
		t.Skip("connection failpoint requires the isolated local mongod fixture")
	}
	db, _ := testDatabase(t)
	ctx := context.Background()
	_, guard := startFixture(t, db)
	var before, after schema.Count
	admin := db.client.Database("admin")
	check(t, admin.RunCommand(ctx, bson.D{
		{Key: "configureFailPoint", Value: "failCommand"},
		{Key: "mode", Value: bson.M{"times": 1}},
		{Key: "data", Value: bson.M{"failCommands": bson.A{"commitTransaction"}, "appName": "cagent-storage", "closeConnection": true}},
	}).Decode(&before))
	t.Cleanup(func() {
		check(t, admin.RunCommand(context.Background(), bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Err())
	})
	request := store.CommitRunRequest{Guard: guard, OperationID: "connection-loss", Status: domain.RunRunning, ExpectedSessionVersion: 2, Events: []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventTextDelta}}}
	result, err := db.CommitRun(ctx, request)
	check(t, err)
	check(t, admin.RunCommand(ctx, bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Decode(&after))
	if after.Count != before.Count+1 {
		t.Fatal("TCP 故障未恰好触发一次")
	}
	replay, err := db.CommitRun(ctx, request)
	check(t, err)
	if !reflect.DeepEqual(result, replay) {
		t.Fatal("断连后重放未返回原提交回执")
	}
	page, err := db.ListEvents(ctx, testScope, "run", store.SequencePage{Limit: 10})
	check(t, err)
	if len(page.Items) != 1 || page.LastSequence != 1 || page.Items[0].Sequence != 1 {
		t.Fatal("断连重试重复写入或跳过事件序号")
	}
	run, err := db.GetRun(ctx, testScope, "run")
	check(t, err)
	if run.Status != domain.RunRunning || run.Version != guard.RunVersion+1 {
		t.Fatal("断连恢复改变了提交次数或运行状态")
	}
}

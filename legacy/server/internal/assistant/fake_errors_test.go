package assistant

import "errors"

// errFake 构造一个普通错误，用于"模型调用失败"这类场景。
// 单独放一个文件，避免各测试文件重复定义同名的辅助函数。
func errFake(msg string) error { return errors.New(msg) }

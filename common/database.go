// Package common 包含通用工具函数和常量定义
package common

// 数据库类型常量
const (
	DatabaseTypeMySQL      = "mysql"    // MySQL 数据库
	DatabaseTypeSQLite     = "sqlite"   // SQLite 数据库
	DatabaseTypePostgreSQL = "postgres" // PostgreSQL 数据库
)

// 数据库类型标记
var UsingSQLite = false             // 是否使用 SQLite 数据库
var UsingPostgreSQL = false         // 是否使用 PostgreSQL 数据库
var LogSqlType = DatabaseTypeSQLite // 日志 SQL 类型，默认为 SQLite
var UsingMySQL = false              // 是否使用 MySQL 数据库
var UsingClickHouse = false         // 是否使用 ClickHouse 数据库

// SQLitePath SQLite 数据库文件路径，_busy_timeout 用于防止锁冲突
var SQLitePath = "one-api.db?_busy_timeout=30000"

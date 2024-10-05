package mysql

import "time"

type User struct {
	ID                  uint       `gorm:"primaryKey;autoIncrement" json:"id" comment:"唯一标识符"`
	UserToken           string     `gorm:"type:varchar(255);unique;not null;collate:utf8mb4_bin" json:"user_token" comment:"用户的唯一令牌"`
	TotalTrafficUsage   uint64     `gorm:"type:bigint unsigned;default:0" json:"total_traffic_usage" comment:"用户已使用的总流量"`
	TotalTrafficQuota   uint64     `gorm:"type:bigint unsigned;not null" json:"total_traffic_quota" comment:"分配给用户的总流量额度"`
	AccountStatus       string     `gorm:"type:enum('active','banned');default:'active';collate:utf8mb4_bin" json:"account_status" comment:"账户状态：active 或 banned"`
	LastLimitTime       *time.Time `gorm:"type:datetime" json:"last_limit_time" comment:"用户最近一次达到流量上限的时间"`
	LastOfflineTime     *time.Time `gorm:"type:datetime" json:"last_offline_time" comment:"用户最近一次下线的时间"`
	AccountCreationDate time.Time  `gorm:"type:datetime;not null;default:CURRENT_TIMESTAMP" json:"account_creation_date" comment:"账户创建日期"`
	Remarks             string     `gorm:"type:text;collate:utf8mb4_bin" json:"remarks" comment:"备注信息"`
}

func (User) TableName() string {
	return "t_user"
}

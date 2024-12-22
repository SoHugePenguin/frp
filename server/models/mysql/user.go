package mysql

import "time"

type User struct {
	ID                  uint      `gorm:"primaryKey;autoIncrement" json:"id" comment:"唯一标识符"`
	Email               string    `gorm:"type:varchar(255);unique;not null;collate:utf8mb4_bin" json:"email" comment:"email"`
	AccountStatus       string    `gorm:"type:enum('active','banned');default:'active';collate:utf8mb4_bin" json:"account_status" comment:"账户状态：active 或 banned"`
	AccountCreationDate time.Time `gorm:"type:datetime;not null;default:CURRENT_TIMESTAMP" json:"account_creation_date" comment:"账户创建日期"`
	Remarks             string    `gorm:"type:text;collate:utf8mb4_bin" json:"remarks" comment:"备注信息"`
}

func (User) TableName() string {
	return "t_user"
}

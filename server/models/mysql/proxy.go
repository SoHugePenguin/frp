package mysql

import "time"

type Proxy struct {
	ID         uint      `gorm:"primaryKey;autoIncrement" json:"id" comment:"唯一标识符"`
	UserID     uint      `gorm:"type:int;not null" json:"user_id" comment:"与t_user关联"`
	ProxyName  string    `gorm:"type:varchar(255);unique;not null;collate:utf8mb4_bin" json:"proxy_name" comment:"代理名称，唯一"`
	ProxyType  string    `gorm:"type:varchar(255);not null;collate:utf8mb4_bin" json:"proxy_type" comment:"代理类型：tcp/udp"`
	RemotePort int       `gorm:"type:int;not null" json:"remote_port" comment:"远程端口"`
	MaxInRate  int64     `gorm:"type:int;not null" json:"max_in_rate" comment:"隧道每秒最大下行字节数"`
	MaxOutRate int64     `gorm:"type:int;not null" json:"max_out_rate" comment:"隧道每秒最大上行字节数"`
	CreateDate time.Time `gorm:"type:datetime;not null;default:CURRENT_TIMESTAMP" json:"create_date" comment:"创建日期"`
	IsDeleted  bool      `gorm:"type:tinyint(1); not null;default:0" json:"is_deleted" comment:"0 false 1 true"`
}

func (Proxy) TableName() string {
	return "t_proxy"
}

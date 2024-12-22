package mysql

import "time"

type ProxyTrafficHistory struct {
	ID              uint      `gorm:"primaryKey;autoIncrement" json:"id" comment:"唯一标识符"`
	ProxyID         uint      `gorm:"type:int;not null" json:"proxy_id" comment:"与t_proxy关联"`
	AvgRateIn       int64     `gorm:"type:bigint;not null" json:"avg_rate_in" comment:"平均下行速率"`
	AvgRateOut      int64     `gorm:"type:bigint;not null" json:"avg_rate_out" comment:"平均上行速率"`
	MaxRateIn       int64     `gorm:"type:bigint;not null" json:"max_rate_in" comment:"最大下行速率"`
	MaxRateOut      int64     `gorm:"type:bigint;not null" json:"max_rate_out" comment:"最大上行速率"`
	TotalTrafficIn  int64     `gorm:"type:bigint;not null" json:"total_traffic_in" comment:"总下行流量"`
	TotalTrafficOut int64     `gorm:"type:bigint;not null" json:"total_traffic_out" comment:"总上行流量"`
	Datetime        time.Time `gorm:"type:datetime;not null;default:CURRENT_TIMESTAMP" json:"datetime" comment:"记录时间"`
}

func (ProxyTrafficHistory) TableName() string {
	return "t_proxy_history"
}

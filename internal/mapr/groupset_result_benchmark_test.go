package mapr

import (
	"fmt"
	"testing"
)

func BenchmarkGroupSetResult(b *testing.B) {
	for _, count := range []int{10, 1000, 10000, 100000} {
		b.Run(fmt.Sprintf("groups%d", count), func(b *testing.B) {
			for _, selection := range []string{"count", "mixed"} {
				fields := "count(*)"
				if selection == "mixed" {
					fields = "host,count(*),sum(v),min(v),max(v),avg(v),len(v),percentage(v),percentile(v)"
				}
				q, err := NewQuery("select "+fields+" from stats group by host order by count(*)", nil)
				if err != nil {
					b.Fatal(err)
				}
				g := NewGroupSet(nil)
				for i := 0; i < count; i++ {
					key := fmt.Sprintf("host-%06d", i)
					set := g.GetSet(key)
					set.Samples = i%7 + 1
					for _, sc := range q.Select {
						set.FValues[sc.FieldStorage] = float64((i * 7919) % count)
						set.SValues[sc.FieldStorage] = key
					}
				}
				for _, order := range []string{"desc", "asc", "none", "ties"} {
					q.OrderBy, q.ReverseOrder = "count(*)", order == "asc"
					if order == "none" {
						q.OrderBy = ""
					}
					if order == "ties" {
						for _, set := range g.sets {
							set.FValues["count(*)"] = 1
						}
					}
					for _, limit := range []int{10, -1} {
						b.Run(fmt.Sprintf("%s/%s/limit%d", selection, order, limit), func(b *testing.B) {
							b.ReportAllocs()
							for n := 0; n < b.N; n++ {
								text, rows, err := g.Result(q, limit, nil)
								if err != nil || rows != count || len(text) == 0 {
									b.Fatalf("rows=%d err=%v", rows, err)
								}
							}
						})
					}
				}
			}
		})
	}
}

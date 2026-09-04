package roomsrv

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func metaTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}

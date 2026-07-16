package gateway

import "testing"

func TestAnnounceREPositive(t *testing.T) {
	for _, s := range []string{
		"Раунд шесть, теперь Москва. Ставлю на 15:51 CEST — через минуту.",
		"Готово, поставила ноту на 25 июля.",
		"Ставлю на 10:41 — через 15 минут от сейчас.",
		"Окей, создала задачу с руками и браузером.",
	} {
		if !announceRE.MatchString(s) {
			t.Fatalf("must match announce language: %q", s)
		}
	}
}

func TestAnnounceRENegative(t *testing.T) {
	for _, s := range []string{
		"Погода в Белграде: 32°C, дождь. Источник: accuweather.com",
		"Могу поставить ноту, если хочешь.",
		"Задача уже стоит с прошлого раза, жду 15:51.",
		"Не смогла достать погоду — браузер вернул пустую страницу.",
	} {
		if announceRE.MatchString(s) {
			t.Fatalf("must NOT match: %q", s)
		}
	}
}

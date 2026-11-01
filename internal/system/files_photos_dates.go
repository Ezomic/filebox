package system

import (
	"time"
)

func (d *dirFiles) loadCustomDates() map[string]string {
	file, err := d.root.Open(ThumbsDir + "/" + customDatesName)
	if err != nil {
		return map[string]string{}
	}
	defer file.Close()

	return decodeCustomDates(file)
}

func (d *dirFiles) SetPhotoDate(name string, when time.Time) error {
	day, err := customDay(when)
	if err != nil {
		return err
	}
	photo, err := d.lookupPhoto(name)
	if err != nil {
		return err
	}
	customDatesMu.Lock()
	defer customDatesMu.Unlock()

	dates := d.loadCustomDates()
	dates[photo.Key] = day.Format(customDayLayout)
	if len(dates) > maxCustomDates {
		if photos, err := d.collectPhotos(); err == nil {
			pruneCustomDates(dates, liveKeys(photos))
		}
	}

	return d.storeCustomDates(dates)
}

func (d *dirFiles) ResetPhotoDate(name string) error {
	photo, err := d.lookupPhoto(name)
	if err != nil {
		return err
	}
	customDatesMu.Lock()
	defer customDatesMu.Unlock()

	dates := d.loadCustomDates()
	if _, ok := dates[photo.Key]; !ok {
		return nil
	}
	delete(dates, photo.Key)

	return d.storeCustomDates(dates)
}

func (d *dirFiles) lookupPhoto(name string) (Photo, error) {
	file, photo, err := d.OpenPhoto(name)
	if err != nil {
		return Photo{}, err
	}
	_ = file.Close()

	return photo, nil
}

func (d *dirFiles) storeCustomDates(dates map[string]string) error {
	data, err := encodeCustomDates(dates)
	if err != nil {
		return err
	}

	return d.writeThumbFile(customDatesName, data)
}

// pruneStaleCustomDates runs with the sweep, which already knows every live photo.
func (d *dirFiles) pruneStaleCustomDates(live map[string]bool) {
	customDatesMu.Lock()
	defer customDatesMu.Unlock()

	dates := d.loadCustomDates()
	if pruneCustomDates(dates, live) {
		_ = d.storeCustomDates(dates)
	}
}

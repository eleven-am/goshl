package domain

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

var timeType = reflect.TypeOf(time.Time{})

func fillValue(value reflect.Value, counter *int) {
	*counter++
	switch value.Kind() {
	case reflect.String:
		value.SetString("value-" + string(rune('a'+*counter%26)))
	case reflect.Int, reflect.Int64:
		value.SetInt(int64(*counter))
	case reflect.Float64:
		value.SetFloat(float64(*counter) + 0.1 + 1e-9)
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Ptr:
		value.Set(reflect.New(value.Type().Elem()))
		fillValue(value.Elem(), counter)
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 2, 2))
		fillValue(value.Index(0), counter)
		fillValue(value.Index(1), counter)
	case reflect.Struct:
		if value.Type() == timeType {
			value.Set(reflect.ValueOf(time.Unix(int64(1_700_000_000+*counter), 123_456_789).UTC()))
			return
		}
		for field := 0; field < value.NumField(); field++ {
			fillValue(value.Field(field), counter)
		}
	default:
		panic("the job contract gained a kind this test cannot fill: " + value.Kind().String())
	}
}

func visitDroppable(path string, value reflect.Value, visit func(string, reflect.Value)) {
	switch value.Kind() {
	case reflect.Ptr:
		visit(path, value)
		visitDroppable(path, value.Elem(), visit)
	case reflect.Slice:
		visit(path, value)
		for index := 0; index < value.Len(); index++ {
			visitDroppable(path+"[]", value.Index(index), visit)
		}
	case reflect.Struct:
		if value.Type() == timeType {
			return
		}
		for field := 0; field < value.NumField(); field++ {
			visitDroppable(path+"."+value.Type().Field(field).Name, value.Field(field), visit)
		}
	default:
		visit(path, value)
	}
}

func filledJob() Job {
	var job Job
	counter := 0
	fillValue(reflect.ValueOf(&job).Elem(), &counter)
	job.Digest = JobDigest(job)
	return job
}

func TestJobDigestDetectsEveryDroppedField(t *testing.T) {
	job := filledJob()
	want := job.Digest
	if len(want) != 64 {
		t.Fatalf("digest %q is not a sha256 hex string", want)
	}
	checked := 0
	visitDroppable("Job", reflect.ValueOf(&job).Elem(), func(path string, value reflect.Value) {
		if path == "Job.Digest" {
			return
		}
		saved := reflect.New(value.Type()).Elem()
		saved.Set(value)
		value.Set(reflect.Zero(value.Type()))
		checked++
		if JobDigest(job) == want {
			t.Errorf("dropping %s leaves the digest unchanged", path)
		}
		value.Set(saved)
	})
	if checked < 60 {
		t.Fatalf("only %d contract values were checked", checked)
	}
}

func TestJobDigestSurvivesJSONAndTimeReencoding(t *testing.T) {
	job := filledJob()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var carried Job
	if err := json.Unmarshal(data, &carried); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(job, carried) {
		t.Fatalf("job changed in a JSON round trip:\n%+v\n%+v", job, carried)
	}
	carried.EnqueuedAt = carried.EnqueuedAt.Truncate(time.Second).Local()
	carried.StartBy = carried.StartBy.Truncate(time.Millisecond)
	if JobDigest(carried) != job.Digest {
		t.Fatal("the digest must not depend on how a coordinator encodes the two times")
	}
}

func TestJobDigestIgnoresItsOwnField(t *testing.T) {
	job := filledJob()
	sealed := job.Digest
	job.Digest = "something else"
	if JobDigest(job) != sealed {
		t.Fatal("the digest must be computed with the Digest field cleared")
	}
}

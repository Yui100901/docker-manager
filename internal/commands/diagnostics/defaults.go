package diagnostics

func defaultLogKeywords() []string {
	return []string{"error", "panic", "exception", "fatal", "oom", "killed"}
}

func defaultHealthOptions() HealthOptions {
	return HealthOptions{
		LogTail:          100,
		RestartThreshold: 3,
		Keywords:         defaultLogKeywords(),
	}
}

func defaultLogsScanOptions() LogsScanOptions {
	return LogsScanOptions{
		Tail:     500,
		Context:  0,
		Keywords: defaultLogKeywords(),
	}
}

func defaultVolumeOptions() VolumeOptions {
	return VolumeOptions{
		SizeMode:  volumeSizeModeAPI,
		SizeImage: volumeDefaultSizeImage,
	}
}

func defaultReportAllOptions() ReportAllOptions {
	return ReportAllOptions{
		LogTail:         200,
		LogKeywords:     defaultLogKeywords(),
		VolumeSizeMode:  volumeSizeModeAPI,
		VolumeSizeImage: volumeDefaultSizeImage,
	}
}

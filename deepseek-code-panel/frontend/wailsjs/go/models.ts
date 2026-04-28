export namespace logstore {
	
	export class LogEntry {
	    id: string;
	    thread_id: string;
	    claude_session_id: string;
	    created_at: string;
	    project_path: string;
	    model: string;
	    permission_mode: string;
	    prompt: string;
	    display_output: string;
	    raw_output: string;
	    exit_code: number;
	    duration_ms: number;
	
	    static createFrom(source: any = {}) {
	        return new LogEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.thread_id = source["thread_id"];
	        this.claude_session_id = source["claude_session_id"];
	        this.created_at = source["created_at"];
	        this.project_path = source["project_path"];
	        this.model = source["model"];
	        this.permission_mode = source["permission_mode"];
	        this.prompt = source["prompt"];
	        this.display_output = source["display_output"];
	        this.raw_output = source["raw_output"];
	        this.exit_code = source["exit_code"];
	        this.duration_ms = source["duration_ms"];
	    }
	}

}

export namespace runner {
	
	export class RunRequest {
	    project_path: string;
	    thread_id: string;
	    claude_session_id: string;
	    prompt: string;
	    api_key: string;
	    base_url: string;
	    model: string;
	    permission_mode: string;
	    language: string;
	
	    static createFrom(source: any = {}) {
	        return new RunRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.project_path = source["project_path"];
	        this.thread_id = source["thread_id"];
	        this.claude_session_id = source["claude_session_id"];
	        this.prompt = source["prompt"];
	        this.api_key = source["api_key"];
	        this.base_url = source["base_url"];
	        this.model = source["model"];
	        this.permission_mode = source["permission_mode"];
	        this.language = source["language"];
	    }
	}

}


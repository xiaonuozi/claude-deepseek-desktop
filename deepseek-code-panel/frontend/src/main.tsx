import React from 'react'
import {createRoot} from 'react-dom/client'
import './style.css'
import App from './App'

const container = document.getElementById('root')

const root = createRoot(container!)

function logCrash(level: string, message: string) {
  try {
    const w = window as any;
    const app = w['go']?.['main']?.['App'];
    if (app && app.WriteAppLog) {
      app.WriteAppLog(`[FRONTEND-${level}] ${message}`);
    }
  } catch {
    // Can't log to backend — likely too early or already tearing down.
  }
}

window.addEventListener('error', (event) => {
  const { message, filename, lineno, colno, error } = event;
  const stack = error instanceof Error ? error.stack : '(no stack)';
  logCrash('ERROR', `${message} at ${filename}:${lineno}:${colno} — ${stack}`);
});

window.addEventListener('unhandledrejection', (event) => {
  const reason = event.reason;
  const stack = reason instanceof Error ? reason.stack : String(reason);
  logCrash('REJECTION', String(reason) + ' — ' + stack);
});

root.render(
    <React.StrictMode>
        <App/>
    </React.StrictMode>
)

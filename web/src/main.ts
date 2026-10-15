import { mount } from 'svelte'
import './app.css'
import App from './App.svelte'
import { startSessionResponder } from './lib/session'

const target = document.getElementById('app')
if (!target) {
  throw new Error('missing #app element')
}

startSessionResponder()
mount(App, { target })

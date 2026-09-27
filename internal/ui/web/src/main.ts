import { mount } from "svelte";
import App from "./App.svelte";
import "./style.css";
import "./privacy.css";
const target = document.getElementById("app");
if (target) mount(App, { target });

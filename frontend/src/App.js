
import './App.css';
import { Route, HashRouter as Router, Routes } from 'react-router-dom';
import {About, Home,Archives, Article, Create, Search} from "./pages";

function App() {
  return (
  <>
    <Router>
      <Routes>
      <Route path={'/'} Component={Home} />
      <Route path={'/about'} Component={About} />
      <Route path={'/archieve'} Component={Archives} />
      <Route path={'/article/create'} Component={Create}/> 
      <Route path={'/article/:articleId'} Component={Article} />
      <Route path={'/search'} Component={Search}/>
      </Routes>
    </Router>
    </>
  );
}

export default App;
